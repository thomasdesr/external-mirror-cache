// Errorutil implements the Wrap and Wrapf functions from github.com/pkg/errors by
// using fmt.Errorf.
package errorutil

import "fmt"

// Wrap annotates `err` with the provided `msg`.
func Wrap(err error, msg string) error {
	// TODO: This should probably return nil if err is nil

	return fmt.Errorf("%v: %w", msg, err)
}

// Wrapf annotates `err` with the provided format string and arguments.
func Wrapf(err error, f string, args ...interface{}) error {
	return Wrap(err, fmt.Sprintf(f, args...))
}
