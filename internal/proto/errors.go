package proto

import "errors"

// RejectReason is the fixed vocabulary of rejection causes.
//
// Error responses carry a reason from this list and nothing else. The client
// slices the first 200 bytes of an error body into its own log on another
// machine, so echoing any part of a request would copy memory content — the
// exact data this project exists to keep local — into a second device's logs.
type RejectReason string

const (
	ReasonProtocolVersion RejectReason = "protocol_version"
	ReasonWrapperShape    RejectReason = "wrapper_shape"
	ReasonDigestMismatch  RejectReason = "digest_mismatch"
	ReasonBodyShape       RejectReason = "body_shape"
	ReasonUnknownKind     RejectReason = "unknown_kind"
	ReasonEntityRev       RejectReason = "entity_rev"
	ReasonTooLarge        RejectReason = "too_large"
	ReasonBadCursor       RejectReason = "bad_cursor"
	ReasonUserMismatch    RejectReason = "user_mismatch"
	ReasonUnauthorized    RejectReason = "unauthorized"
	ReasonStorageFull     RejectReason = "storage_full"
	ReasonBadRequest      RejectReason = "bad_request"
	ReasonOverloaded      RejectReason = "overloaded"
	ReasonInternal        RejectReason = "internal"
)

// RejectError carries a reason and, by construction, nothing else. The struct
// has exactly one field so there is nowhere for request bytes to hide.
type RejectError struct {
	Reason RejectReason
}

func (e *RejectError) Error() string { return string(e.Reason) }

// Reject builds a RejectError.
func Reject(r RejectReason) error { return &RejectError{Reason: r} }

// ReasonOf extracts the reason from err, or ReasonInternal if err is not a
// RejectError. Handlers use this so an unexpected error can never leak its text.
func ReasonOf(err error) RejectReason {
	var re *RejectError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ReasonInternal
}
