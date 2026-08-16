package proto

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// The client copies the first 200 bytes of an error body into its own log on
// another machine. A RejectError with a second field would eventually carry
// request data there, so the shape itself is the guarantee.
func TestRejectErrorHasExactlyOneField(t *testing.T) {
	tp := reflect.TypeOf(RejectError{})
	if tp.NumField() != 1 {
		t.Fatalf("RejectError has %d fields; it must carry only a reason", tp.NumField())
	}
	if tp.Field(0).Name != "Reason" {
		t.Fatalf("RejectError field is %q, want Reason", tp.Field(0).Name)
	}
	if tp.Field(0).Type != reflect.TypeOf(RejectReason("")) {
		t.Fatal("RejectError.Reason must be a RejectReason")
	}
}

func TestReasonOfExtracts(t *testing.T) {
	err := Reject(ReasonDigestMismatch)
	if got := ReasonOf(err); got != ReasonDigestMismatch {
		t.Fatalf("ReasonOf = %q", got)
	}
	if got := ReasonOf(fmt.Errorf("wrapped: %w", err)); got != ReasonDigestMismatch {
		t.Fatalf("ReasonOf through a wrap = %q", got)
	}
}

// An unexpected error must degrade to a generic reason rather than surfacing
// its text, which could contain a file path, a query, or body content.
func TestReasonOfUnknownErrorIsInternal(t *testing.T) {
	if got := ReasonOf(errors.New("sqlite: no such column: secret_value")); got != ReasonInternal {
		t.Fatalf("ReasonOf(unknown) = %q, want %q", got, ReasonInternal)
	}
	if got := ReasonOf(nil); got != ReasonInternal {
		t.Fatalf("ReasonOf(nil) = %q", got)
	}
}

func TestErrorTextIsTheReasonOnly(t *testing.T) {
	err := Reject(ReasonTooLarge)
	if err.Error() != string(ReasonTooLarge) {
		t.Fatalf("Error() = %q, want the bare reason", err.Error())
	}
}
