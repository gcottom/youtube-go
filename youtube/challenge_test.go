package youtube

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// This synthetic player has the two URL transformations EJS looks for.
const testChallengePlayer = `
var outer;
(function () {
  var window = this;
  if (typeof process !== 'undefined' || typeof require !== 'undefined') {
    throw new Error('player can access the host runtime');
  }
  function URLState() {
    this.values = Object.create(null);
  }
  URLState.prototype.set = function (key, val) { this.values[key] = val; };
  URLState.prototype.get = function (key) { return this.values[key]; };
  URLState.prototype.transform = function () {
    if (this.values.n === 'bad') throw new Error('rejected n value');
    if (this.values.n) this.values.n = this.values.n + '_n';
    if (this.values.s) {
      const value = decodeURIComponent(this.values.s);
      this.values.s = encodeURIComponent(value.split('').reverse().join(''));
    }
  };
  function makeURL(url, key, value) {
    const state = new URLState();
    state.set('alr', 'yes');
    state.set(key, value);
    return state;
  }
})();`

func TestNodeSolverBatchesChallenges(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable")
	}
	if err := exec.Command(node, "--permission", "-e", "").Run(); err != nil {
		t.Skip("Node.js has no stable permission flag")
	}
	solver := NodeSolver{Executable: node}
	t.Run("both types", func(t *testing.T) {
		got, err := solver.Solve(context.Background(), []byte(testChallengePlayer), []string{"ab%", "xy=", "ab%"}, []string{"tok", "__proto__"})
		if err != nil {
			t.Fatal(err)
		}
		want := ChallengeAnswers{
			Signatures: map[string]string{"ab%": "%ba", "xy=": "=yx"},
			Throttles:  map[string]string{"tok": "tok_n", "__proto__": "__proto___n"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("partial answers", func(t *testing.T) {
		got, err := solver.Solve(context.Background(), []byte(testChallengePlayer), []string{"abc"}, []string{"bad"})
		if err == nil || !strings.Contains(err.Error(), "rejected n value") {
			t.Fatalf("got %v, want a throttle challenge error", err)
		}
		if got.Signatures["abc"] != "cba" {
			t.Fatalf("signature result was lost: %+v", got)
		}
	})
}

func TestNodeSolverCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (NodeSolver{}).Solve(ctx, []byte(testChallengePlayer), []string{"abc"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
