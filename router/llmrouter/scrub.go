package llmrouter

import "github.com/Loomux/server/credentials"

// scrubKeys keeps every router key (credentials' system secrets,
// LOOM-185) out of err's text: a provider may echo the key it was sent
// in its error, which then reaches logs and a dispatch's stored error.
// errors.Is and errors.As still see through it.
func scrubKeys(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if scrubbed := credentials.RedactValues(msg, nil); scrubbed != msg {
		return &scrubbedError{err: err, msg: scrubbed}
	}
	return err
}

type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }
