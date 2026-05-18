package shell

import "errors"

// stdErrorsAs is `errors.As` re-exported under an internal name so the
// rest of the package doesn't need to import "errors" everywhere.
func stdErrorsAs(err error, target any) bool { return errors.As(err, target) }
