package repository

import "errors"

var ErrNotFound = errors.New("not found")

// ErrAlreadyReviewed indicates that a shift request has left the pending state,
// so a concurrent or duplicate review cannot claim it again.
var ErrAlreadyReviewed = errors.New("shift request already reviewed")
