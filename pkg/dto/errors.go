package dto

import "github.com/devman-project/devman/pkg/errs"

// FromError converts any error into the wire form. Nil in, nil out.
func FromError(err error) *Error {
	if err == nil {
		return nil
	}
	de := errs.From(err)
	return &Error{
		Code:    string(de.Code),
		Message: de.Message,
		Path:    de.Path,
		Details: de.Details,
	}
}

// FromErrors converts a slice of errors, skipping nils.
func FromErrors(list []error) []Error {
	out := make([]Error, 0, len(list))
	for _, err := range list {
		if converted := FromError(err); converted != nil {
			out = append(out, *converted)
		}
	}
	return out
}
