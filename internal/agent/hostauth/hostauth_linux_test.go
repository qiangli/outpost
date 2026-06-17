//go:build linux

package hostauth

import (
	"strings"
	"testing"
)

// crypt(3) hashes of the password "testpw123" (salt "abcdefgh"), one per
// algorithm. If a blank algorithm import in hostauth_linux.go is dropped,
// crypt.NewFromHash returns nil for that prefix and the matching subtest
// fails — guarding against a silent "every password rejected" regression.
const shadowPassword = "testpw123"

var shadowHashes = map[string]string{
	"md5":    "$1$abcdefgh$uB.i5i2b2MXkibsKcOwgP0",
	"sha256": "$5$abcdefgh$yGzV0eEnYagmSSWhLpOQWUmb07mcx9RbELSXcovuOKB",
	"sha512": "$6$abcdefgh$s/Lh7cJ1IB17oGZWZExU3L.JsMvriqiXlDOmJ.v/k/QCThkdWW6A8FVZzH7OFV9QFwFIbfwLyc7IxyERAMJB6.",
}

func TestVerifyShadow_AllAlgorithms(t *testing.T) {
	for algo, hash := range shadowHashes {
		t.Run(algo, func(t *testing.T) {
			shadow := "root:!:0:::::\nalice:" + hash + ":19000:0:99999:7:::\n"

			if err := verifyShadow(strings.NewReader(shadow), "alice", shadowPassword); err != nil {
				t.Errorf("correct password rejected for %s: %v", algo, err)
			}
			if err := verifyShadow(strings.NewReader(shadow), "alice", "wrongpw"); err != ErrInvalidCredentials {
				t.Errorf("wrong password: want ErrInvalidCredentials, got %v", err)
			}
		})
	}
}

func TestVerifyShadow_LockedAndMissing(t *testing.T) {
	cases := map[string]string{
		"locked-bang":   "alice:!:19000:0:99999:7:::\n",
		"locked-star":   "alice:*:19000:0:99999:7:::\n",
		"empty-hash":    "alice::19000:0:99999:7:::\n",
		"absent-user":   "bob:" + shadowHashes["sha512"] + ":19000:0:99999:7:::\n",
		"malformed-row": "alice\n",
	}
	for name, shadow := range cases {
		t.Run(name, func(t *testing.T) {
			if err := verifyShadow(strings.NewReader(shadow), "alice", shadowPassword); err != ErrInvalidCredentials {
				t.Errorf("want ErrInvalidCredentials, got %v", err)
			}
		})
	}
}

func TestLinuxAuth_RejectsEmptyCredentials(t *testing.T) {
	a := linuxAuth{}
	if err := a.Authenticate("", "pw"); err != ErrInvalidCredentials {
		t.Errorf("empty user: want ErrInvalidCredentials, got %v", err)
	}
	if err := a.Authenticate("alice", ""); err != ErrInvalidCredentials {
		t.Errorf("empty pass: want ErrInvalidCredentials, got %v", err)
	}
}
