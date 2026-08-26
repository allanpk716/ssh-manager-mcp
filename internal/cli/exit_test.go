package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestExitCodeFor(t *testing.T) {
	if got := ExitCodeFor(nil); got != 0 {
		t.Errorf("nil → 0, got %d", got)
	}
	if got := ExitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("plain error → 1, got %d", got)
	}
	if got := ExitCodeFor(NewExitCodeError(2, errors.New("x"))); got != 2 {
		t.Errorf("pinned code 2 → 2, got %d", got)
	}
	// Hand-rolled literals bypassing the constructor must degrade to 1 —
	// never "error but exit 0", never OS-truncated garbage.
	if got := ExitCodeFor(&ExitCodeError{Code: 0, Err: errors.New("x")}); got != 1 {
		t.Errorf("literal code 0 → 1, got %d", got)
	}
	if got := ExitCodeFor(&ExitCodeError{Code: 999, Err: errors.New("x")}); got != 1 {
		t.Errorf("literal code 999 → 1, got %d", got)
	}
}

func TestNewExitCodeErrorInvariants(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"code 0", 0, errors.New("x")},
		{"code 999", 999, errors.New("x")},
		{"nil err", 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", tc.name)
				}
			}()
			NewExitCodeError(tc.code, tc.err)
		})
	}
}

func TestExitCodeErrorNilErrRendering(t *testing.T) {
	e := &ExitCodeError{Code: 1} // hand-rolled, Err nil — must not panic at print time
	if e.Error() == "" {
		t.Error("nil-Err literal must still render a non-empty message")
	}
}

// TestExitCodeForCrossesCobra proves the code survives a cobra Execute round
// trip — any command can pin its exit code.
func TestExitCodeForCrossesCobra(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(cmd *cobra.Command, args []string) error {
			return NewExitCodeError(2, fmt.Errorf("internal"))
		},
	})
	root.SetArgs([]string{"boom"})
	err := root.Execute()
	if err == nil {
		t.Fatal("RunE error must surface")
	}
	if got := ExitCodeFor(err); got != 2 {
		t.Fatalf("code 2 must cross cobra, got %d", got)
	}
}
