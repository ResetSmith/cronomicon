package secrets

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 2.5.1 SG2.10: every production seal is bound to its row. Once BindSeals has
// run, an unbound value written by a forgotten caller would not open, so this
// holds production code (not tests) to the bound calls: EncryptSetting and
// DecryptSetting (or OpenSetting) for a settings token, Sealer.Seal and
// Sealer.Open with a Binding for a row. The unbound forms are for BindSeals,
// which reads the old values, and for tests.
func TestProductionSealsAreBound(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := regexp.MustCompile(`secrets\.EncryptString\(|secrets\.DecryptString\(|\.SealLegacy\(`)
	// Inside this package: the unbound primitives are BindSeals' and the
	// token functions' own.
	inPackage := regexp.MustCompile(`\bDecryptString\(|\bEncryptString\(|\benvelopeEncrypt\(|\benvelopeDecrypt\(`)
	allowedInPackage := map[string]bool{
		"blob.go":      true, // defines the token functions
		"kek.go":       true, // defines the primitives
		"seal.go":      true, // SealLegacy, for tests
		"bindseals.go": true, // reads the values sealed before 2.5.1
	}
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "web", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inSecrets := filepath.Base(filepath.Dir(path)) == "secrets" && strings.Contains(path, filepath.Join("internal", "secrets"))
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if forbidden.MatchString(line) {
				bad = append(bad, path+":"+itoa(i+1)+": "+trimmed)
			}
			if inSecrets && !allowedInPackage[filepath.Base(path)] && inPackage.MatchString(line) {
				bad = append(bad, path+":"+itoa(i+1)+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Errorf("an unbound seal or open in production code (use EncryptSetting/DecryptSetting or Sealer.Seal/Open with a Binding): %s", b)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
