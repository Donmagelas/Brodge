package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeSigningFixture(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
}

func signingFixture(t *testing.T) (string, string, []string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	signing := filepath.Join(root, "signing")
	for name, data := range map[string]string{"identity.keychain-db": "private-keychain", "keychain-password": "fake-test-password\n", "certificate.pem": "certificate", "identity.sha1": strings.Repeat("A", 40) + "\n"} {
		writeSigningFixture(t, filepath.Join(signing, name), data)
	}
	writeSigningFixture(t, filepath.Join(bin, "uname"), "#!/bin/sh\necho Darwin\n")
	writeSigningFixture(t, filepath.Join(bin, "openssl"), "#!/bin/sh\necho 'SHA1 Fingerprint="+strings.Repeat("A", 40)+"'\n")
	writeSigningFixture(t, filepath.Join(bin, "security"), `#!/bin/sh
printf '%s\n' "$*" >> "$SIGNING_TEST_SECURITY_LOG"
if [ "$1" = list-keychains ] && [ "$#" -eq 3 ]; then printf '    "/existing/login.keychain-db"\n'; fi
`)
	writeSigningFixture(t, filepath.Join(bin, "codesign"), `#!/bin/sh
printf '%s\n' "$*" >> "$SIGNING_TEST_CODESIGN_LOG"
exit "${SIGNING_TEST_FAIL:-0}"
`)
	env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "BRODGE_SIGNING_DIR="+signing, "BRODGE_CODESIGN_IDENTITY=", "SIGNING_TEST_SECURITY_LOG="+filepath.Join(root, "security.log"), "SIGNING_TEST_CODESIGN_LOG="+filepath.Join(root, "codesign.log"))
	return root, signing, env
}

func TestSigningReusesPersistentIdentityAndExistingKeychains(t *testing.T) {
	root, signing, env := signingFixture(t)
	for _, target := range []string{"first.app", "second.app"} {
		cmd := exec.Command("sh", "../../scripts/sign-macos.sh", filepath.Join(root, target))
		cmd.Env = env
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sign failed: %v %s", err, output)
		}
	}
	log, err := os.ReadFile(filepath.Join(root, "codesign.log"))
	if err != nil || strings.Count(string(log), "--sign "+strings.Repeat("A", 40)) != 2 || strings.Contains(string(log), "--sign - ") {
		t.Fatal("builds did not reuse the persistent certificate", string(log), err)
	}
	security, _ := os.ReadFile(filepath.Join(root, "security.log"))
	if strings.Contains(string(security), "create-keychain") || strings.Contains(string(security), "default-keychain") || !strings.Contains(string(security), "list-keychains -d user -s /existing/login.keychain-db "+filepath.Join(signing, "identity.keychain-db")) {
		t.Fatal("signing replaced identity or existing keychain configuration", string(security))
	}
}

func TestSigningFailsClosedWithoutRegeneratingIdentity(t *testing.T) {
	for _, scenario := range []string{"missing", "mismatch", "signer-failed", "ad-hoc"} {
		t.Run(scenario, func(t *testing.T) {
			root, signing, env := signingFixture(t)
			switch scenario {
			case "missing":
				if err := os.Remove(filepath.Join(signing, "identity.sha1")); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				writeSigningFixture(t, filepath.Join(signing, "identity.sha1"), strings.Repeat("B", 40)+"\n")
			case "signer-failed":
				env = append(env, "SIGNING_TEST_FAIL=7")
			case "ad-hoc":
				env = append(env, "BRODGE_CODESIGN_IDENTITY=-")
			}
			cmd := exec.Command("sh", "../../scripts/sign-macos.sh", filepath.Join(root, "Brodge.app"))
			cmd.Env = env
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatal("invalid signing succeeded", string(output))
			}
			log, _ := os.ReadFile(filepath.Join(root, "codesign.log"))
			want := 0
			if scenario == "signer-failed" {
				want = 1
			}
			if strings.Count(string(log), "--sign ") != want || strings.Contains(string(log), "--sign - ") {
				t.Fatal("signer failure fell back to another identity", string(log))
			}
			security, _ := os.ReadFile(filepath.Join(root, "security.log"))
			if strings.Contains(string(security), "create-keychain") || strings.Contains(string(security), "import") {
				t.Fatal("broken identity silently regenerated")
			}
		})
	}
}

func TestSigningSetupTrustDomainIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name, domain, uid, want string
	}{
		{"default-user", "", "501", "add-trusted-cert -r trustRoot -p codeSign"},
		{"disposable-ci-admin", "admin", "0", "add-trusted-cert -d -r trustRoot -p codeSign"},
		{"admin-not-elevated", "admin", "501", ""},
		{"unknown-domain", "other", "0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, env := signingFixture(t)
			writeSigningFixture(t, filepath.Join(root, "bin/id"), "#!/bin/sh\necho "+tc.uid+"\n")
			cmd := exec.Command("sh", "../../scripts/sign-macos.sh", "--setup")
			cmd.Env = append(env, "BRODGE_SIGNING_TRUST_DOMAIN="+tc.domain)
			output, err := cmd.CombinedOutput()
			if (err == nil) != (tc.want != "") {
				t.Fatalf("unexpected setup result: %v %s", err, output)
			}
			log, _ := os.ReadFile(filepath.Join(root, "security.log"))
			if tc.want == "" {
				if strings.Contains(string(log), "add-trusted-cert") {
					t.Fatal("invalid setup changed trust")
				}
			} else if !strings.Contains(string(log), tc.want) {
				t.Fatal("wrong trust domain or policy", string(log))
			}
		})
	}
}

func TestPackagingReplacesOnlyStoppedAppWithSameIdentity(t *testing.T) {
	for _, failure := range []string{"none", "running", "signing", "identity-changed"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			data, err := os.ReadFile("../../scripts/package-macos.sh")
			if err != nil {
				t.Fatal(err)
			}
			writeSigningFixture(t, filepath.Join(root, "scripts/package-macos.sh"), string(data))
			writeSigningFixture(t, filepath.Join(root, "Brodge"), "new binary")
			old := filepath.Join(root, "build/Brodge.app/Contents/MacOS/Brodge")
			writeSigningFixture(t, old, "old binary")
			bin := filepath.Join(root, "bin")
			writeSigningFixture(t, filepath.Join(bin, "lsof"), "#!/bin/sh\n[ \"$PACKAGING_TEST_FAILURE\" = running ]\n")
			writeSigningFixture(t, filepath.Join(bin, "sips"), "#!/bin/sh\nexit 0\n")
			writeSigningFixture(t, filepath.Join(bin, "iconutil"), "#!/bin/sh\nexit 0\n")
			writeSigningFixture(t, filepath.Join(root, "scripts/sign-macos.sh"), "#!/bin/sh\n[ \"$PACKAGING_TEST_FAILURE\" != signing ]\n")
			writeSigningFixture(t, filepath.Join(bin, "codesign"), `#!/bin/sh
if [ "$1" = -d ]; then
 echo 'designated => identifier "local.brodge.desktop" and certificate leaf = H"old"'
 exit 0
fi
# codesign treats -R as a file path unless a literal requirement starts with =.
[ "$3" = -R ] && [ "$4" = '=identifier "local.brodge.desktop" and certificate leaf = H"old"' ] || exit 9
[ "$PACKAGING_TEST_FAILURE" != identity-changed ]
`)
			cmd := exec.Command("sh", filepath.Join(root, "scripts/package-macos.sh"))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PACKAGING_TEST_FAILURE="+failure)
			output, err := cmd.CombinedOutput()
			if (err == nil) != (failure == "none") {
				t.Fatalf("unexpected package result: %v %s", err, output)
			}
			actual, err := os.ReadFile(old)
			want := "old binary"
			if failure == "none" {
				want = "new binary"
			}
			if err != nil || string(actual) != want {
				t.Fatal("packaging did not preserve or replace the application correctly", err)
			}
		})
	}
}
