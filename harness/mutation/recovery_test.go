//go:build linux || darwin

package mutation

import (
	"errors"
	"testing"
)

func TestRecoverNeverReplansInterruptedOrCompletedMutation(t *testing.T) {
	for _, stage := range []string{"before_replace", "before_receipt", "complete"} {
		t.Run(stage, func(t *testing.T) {
			s := service(t)
			if _, found, err := s.Recover(t.Context(), "operation"); found || err != nil {
				t.Fatalf("missing receipt=%t %v", found, err)
			}
			if stage != "complete" {
				s.hook = func(point string, _ int) error {
					if point == stage {
						return errors.New("interrupted")
					}
					return nil
				}
			}
			outcome := s.Execute(t.Context(), "operation", request("file", Revision{}, "contents"))
			recovered, found, err := s.Recover(t.Context(), "operation")
			if !found || err != nil {
				t.Fatalf("receipt=%t %v", found, err)
			}
			if stage == "before_receipt" {
				if recovered.Code != Indeterminate {
					t.Fatal(recovered)
				}
			} else if recovered.Code != outcome.Code {
				t.Fatalf("recovered=%+v outcome=%+v", recovered, outcome)
			}
			s.hook = nil
			again, found, err := s.Recover(t.Context(), "operation")
			if !found || err != nil || again.Code != recovered.Code {
				t.Fatal("reading receipt changed its outcome")
			}
		})
	}
}
