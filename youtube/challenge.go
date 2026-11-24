package youtube

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// ChallengeSolver resolves YouTube's signature and n-throttle challenges in a batch.
// A solver may return partial answers alongside an error.
type ChallengeSolver interface {
	Solve(ctx context.Context, player []byte, signatures, throttles []string) (ChallengeAnswers, error)
}

// ChallengeAnswers maps encrypted inputs to their corresponding player outputs.
type ChallengeAnswers struct {
	Signatures map[string]string
	Throttles  map[string]string
}

// NodeSolver runs the bundled yt-dlp EJS solver with Node.js 22.13 or later.
// Executable defaults to node on PATH. Pass only YouTube player scripts to Solve.
type NodeSolver struct {
	Executable string
}

//go:embed ejs/yt.solver.lib.min.js
var ejsLibrary string

//go:embed ejs/yt.solver.core.min.js
var ejsCore string

//go:embed ejs/NOTICE.txt
var ejsNotice string

const (
	maxPlayerBytes        = 16 << 20
	maxChallenges         = 1024
	maxChallengeBytes     = 4096
	challengeTimeout      = 15 * time.Second
	maxChallengeOutput    = 8 << 20
	maxChallengeErrorSize = 4096
)

type challengeRequest struct {
	Type       string   `json:"type"`
	Challenges []string `json:"challenges"`
}

type challengeInput struct {
	Type     string             `json:"type"`
	Player   string             `json:"player"`
	Requests []challengeRequest `json:"requests"`
}

type challengeResult struct {
	Type  string            `json:"type"`
	Error string            `json:"error"`
	Data  map[string]string `json:"data"`
}

type challengeOutput struct {
	Type      string            `json:"type"`
	Error     string            `json:"error"`
	Responses []challengeResult `json:"responses"`
}

// Solve evaluates the player scripts in a separate Node.js process with permissions restricted.
func (s NodeSolver) Solve(ctx context.Context, player []byte, signatures, throttles []string) (ChallengeAnswers, error) {
	var answers ChallengeAnswers
	if len(signatures)+len(throttles) == 0 {
		return answers, nil
	}
	if err := ctx.Err(); err != nil {
		return answers, err
	}
	if len(player) == 0 || len(player) > maxPlayerBytes {
		return answers, fmt.Errorf("YouTube player script must contain between 1 and %d bytes", maxPlayerBytes)
	}
	if len(signatures)+len(throttles) > maxChallenges {
		return answers, fmt.Errorf("too many YouTube player challenges (maximum %d)", maxChallenges)
	}
	destinations := []struct {
		inputs  []string
		kind    string
		answers *map[string]string
	}{
		{signatures, "sig", &answers.Signatures},
		{throttles, "n", &answers.Throttles},
	}
	input := challengeInput{Type: "player", Player: string(player)}
	for _, destination := range destinations {
		if len(destination.inputs) == 0 {
			continue
		}
		seen := make(map[string]bool, len(destination.inputs))
		request := challengeRequest{Type: destination.kind}
		for _, value := range destination.inputs {
			if value == "" || len(value) > maxChallengeBytes {
				return answers, fmt.Errorf("invalid YouTube %s challenge length", destination.kind)
			}
			if !seen[value] {
				seen[value] = true
				request.Challenges = append(request.Challenges, value)
			}
		}
		input.Requests = append(input.Requests, request)
	}
	output, err := s.run(ctx, input)
	if err != nil {
		return answers, err
	}
	var requestErrors []error
	index := 0
	for _, destination := range destinations {
		if len(destination.inputs) == 0 {
			continue
		}
		request, response := input.Requests[index], output.Responses[index]
		index++
		if response.Type != "result" {
			requestErrors = append(requestErrors, fmt.Errorf("solve YouTube %s challenge: %s", request.Type, challengeErrorLine(response.Error)))
			continue
		}
		values := make(map[string]string, len(request.Challenges))
		for _, challenge := range request.Challenges {
			if value := response.Data[challenge]; value != "" {
				values[challenge] = value
			}
		}
		*destination.answers = values
		if missing := len(request.Challenges) - len(values); missing > 0 {
			requestErrors = append(requestErrors, fmt.Errorf("YouTube player returned no answer for %d %s challenge(s)", missing, request.Type))
		}
	}
	return answers, errors.Join(requestErrors...)
}

func (s NodeSolver) run(ctx context.Context, input challengeInput) (challengeOutput, error) {
	var output challengeOutput
	// The request is encoded as JSON and parsed inside the player context.
	// Using JSON.parse also preserves special object keys as data.
	request, err := json.Marshal(input)
	if err != nil {
		return output, fmt.Errorf("encode YouTube player challenges: %w", err)
	}
	quotedRequest, err := json.Marshal(string(request))
	if err != nil {
		return output, fmt.Errorf("quote YouTube player challenges: %w", err)
	}
	program := strings.Join([]string{
		"/*\n" + ejsNotice + "\n*/",
		ejsLibrary,
		"Object.assign(globalThis, lib);",
		ejsCore,
		"try { JSON.stringify(jsc(JSON.parse(" + string(quotedRequest) + "))); }" +
			" catch (error) { JSON.stringify({type: 'error', error: String(error).slice(0, 512)}); }",
	}, "\n")
	quotedProgram, err := json.Marshal(program)
	if err != nil {
		return output, fmt.Errorf("quote YouTube solver program: %w", err)
	}
	// A separate VM context keeps host objects, including process and require, out of player scope.
	// The subprocess also uses Node's permission model and runs without inherited NODE_OPTIONS.
	source := "const vm = require('node:vm');\n" +
		"try {\n" +
		"  const output = new vm.Script(" + string(quotedProgram) + ", { filename: 'youtube-ejs.js' })\n" +
		"    .runInNewContext(Object.create(null), { timeout: 10000, microtaskMode: 'afterEvaluate' });\n" +
		"  if (typeof output !== 'string') throw new Error('solver returned no output');\n" +
		"  process.stdout.write(output);\n" +
		"} catch {\n" +
		"  process.stderr.write('could not evaluate the YouTube player within the Node process');\n" +
		"  process.exitCode = 1;\n" +
		"}\n"
	node := s.Executable
	if node == "" {
		node = "node"
	}
	runCtx, cancel := context.WithTimeout(ctx, challengeTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, node, "--permission", "--max-old-space-size=256", "--input-type=commonjs", "-")
	cmd.WaitDelay = time.Second
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(source)
	stdout := challengeBuffer{limit: maxChallengeOutput}
	stderr := challengeBuffer{limit: maxChallengeErrorSize}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return output, fmt.Errorf("solve YouTube challenges: %w", runCtx.Err())
		}
		if message := challengeErrorLine(stderr.buffer.String()); message != "no error details" {
			return output, fmt.Errorf("solve YouTube challenges with Node.js 22.13 or later: %w (%s)", err, message)
		}
		return output, fmt.Errorf("solve YouTube challenges with Node.js 22.13 or later: %w", err)
	}
	if stdout.overflow {
		return output, errors.New("YouTube challenge solver response exceeded its size limit")
	}
	if err := json.Unmarshal(stdout.buffer.Bytes(), &output); err != nil {
		return output, fmt.Errorf("decode YouTube challenge solver response: %w", err)
	}
	if output.Type != "result" {
		return output, fmt.Errorf("YouTube challenge solver: %s", challengeErrorLine(output.Error))
	}
	if len(output.Responses) != len(input.Requests) {
		return output, errors.New("YouTube challenge solver returned an unexpected number of responses")
	}
	return output, nil
}

// challengeBuffer drains the process pipe while capping the bytes retained.
type challengeBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *challengeBuffer) Write(data []byte) (int, error) {
	length := len(data)
	if remaining := b.limit - b.buffer.Len(); length > remaining {
		data = data[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(data)
	return length, nil
}

func challengeErrorLine(message string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	if len(line) > 512 {
		line = line[:512] + "..."
	}
	line = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, line)
	if line == "" {
		return "no error details"
	}
	return line
}
