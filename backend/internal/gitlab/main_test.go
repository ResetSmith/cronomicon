package gitlab

import (
	"os"
	"testing"
)

// This package's tests reach a second repository as a directory on the test's
// own disk. An agency's repository is otherwise held to http or https on an
// allowed host (GR-10, Service.checkURL); a URL with a scheme is held to it
// here too, which is what the tests of the rule use.
func TestMain(m *testing.M) {
	testLocalRemotes = true
	os.Exit(m.Run())
}
