package jsonrpc

import (
	"sort"

	"github.com/swaggest/usecase/status"
)

// ErrWithFields exposes structured context of error.
type ErrWithFields interface {
	error
	Fields() map[string]interface{}
}

// ErrWithAppCode exposes application error code.
type ErrWithAppCode interface {
	error
	AppErrCode() int
}

// ErrWithCanonicalStatus exposes canonical status code.
type ErrWithCanonicalStatus interface {
	error
	Status() status.Code
}

// ValidationErrors is a list of validation errors.
//
// Key is field position (e.g. "path:id" or "body"), value is a list of issues with the field.
type ValidationErrors map[string][]string

// Error returns error message.
func (re ValidationErrors) Error() string {
	return "validation failed"
}

// Fields returns request errors by field location and name.
func (re ValidationErrors) Fields() map[string]interface{} {
	res := make(map[string]interface{}, len(re))

	for k, v := range re {
		sort.Strings(v)
		res[k] = v
	}

	return res
}
