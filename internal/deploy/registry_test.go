package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRegistry(t *testing.T) {
	ok := map[string]string{
		"":                               "",
		"  ":                             "",
		"ghcr.io/versolauth":             "", // the default spelled out is no override
		"cr.yandex/crp1a2b3c":            "cr.yandex/crp1a2b3c",
		"registry.example.com:5000/team": "registry.example.com:5000/team",
		"localhost:5000":                 "localhost:5000",
		"ghcr.io/versolauth/mirror":      "ghcr.io/versolauth/mirror",
		"docker.io/my-org/sub_dir.x":     "docker.io/my-org/sub_dir.x",
	}
	for in, want := range ok {
		got, err := ParseRegistry(in)
		if err != nil || got != want {
			t.Errorf("ParseRegistry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"https://cr.yandex/crp1a2b3c", // a scheme
		"cr.yandex/crp1a2b3c/",        // trailing slash
		"CR.yandex/Team",              // upper case: not a valid repository path
		"cr.yandex//team",
		"cr yandex/team",
		"cr.yandex/team:latest", // a tag is the version's job
		"-bad.host/team",
	} {
		if got, err := ParseRegistry(in); err == nil {
			t.Errorf("ParseRegistry(%q) = %q, want an error", in, got)
		}
	}
}

func TestToolsImageNamesTheRegistry(t *testing.T) {
	if got := ToolsImage("", "0.7.2"); got != "ghcr.io/versolauth/versola-tools:0.7.2" {
		t.Errorf("default: %q", got)
	}
	if got := ToolsImage("cr.yandex/crp1a2b3c", "test-42"); got != "cr.yandex/crp1a2b3c/versola-tools:test-42" {
		t.Errorf("custom: %q", got)
	}
}

const registryCompose = `name: versola-vps
services:
  openbao:
    image: openbao/openbao:2.5.4
  central:
    image: %REG%/versola-central:%TAG%
  auth:
    image: "%REG%/versola-auth:%TAG%"
  edge:
    image: %REG%/versola-edge:%TAG%
  migrate:
    image: %REG%/versola-tools:%TAG%
  mtls-init:
    image: smallstep/step-ca:0.28.4
`

func writeCompose(t *testing.T, reg, tag string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.fragment.yml")
	body := strings.NewReplacer("%REG%", reg, "%TAG%", tag).Replace(registryCompose)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequireRegistryImages(t *testing.T) {
	const reg = "cr.yandex/crp1a2b3c"

	t.Run("the default registry is not checked", func(t *testing.T) {
		// Not even read: no such file.
		if err := requireRegistryImages(filepath.Join(t.TempDir(), "nope.yml"), "", "0.7.2"); err != nil {
			t.Errorf("error for the default registry: %v", err)
		}
	})
	t.Run("images under the registry at the version pass", func(t *testing.T) {
		if err := requireRegistryImages(writeCompose(t, reg, "test-42"), reg, "test-42"); err != nil {
			t.Errorf("error: %v", err)
		}
	})
	t.Run("a tools release that ignored the registry is refused", func(t *testing.T) {
		err := requireRegistryImages(writeCompose(t, "ghcr.io/versolauth", "test-42"), reg, "test-42")
		if err == nil || !strings.Contains(err.Error(), "ignores --registry") || !strings.Contains(err.Error(), "ghcr.io/versolauth/versola-central:test-42") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a registry that is a path prefix of the default is not mistaken for it", func(t *testing.T) {
		// ghcr.io/versolauth/mirror/... starts with ghcr.io/versolauth/ but is
		// under the registry that was asked for.
		const mirror = "ghcr.io/versolauth/mirror"
		if err := requireRegistryImages(writeCompose(t, mirror, "1"), mirror, "1"); err != nil {
			t.Errorf("error: %v", err)
		}
		if err := requireRegistryImages(writeCompose(t, "ghcr.io/versolauth", "1"), mirror, "1"); err == nil {
			t.Error("the default registry's images passed for the mirror")
		}
	})
	t.Run("a registry that is a leading part of the default path is not mistaken for it", func(t *testing.T) {
		// An old tools writes ghcr.io/versolauth/versola-auth; that is not
		// ghcr.io/versola-auth.
		for _, short := range []string{"ghcr.io", "ghcr.io/versola"} {
			if err := requireRegistryImages(writeCompose(t, "ghcr.io/versolauth", "1"), short, "1"); err == nil {
				t.Errorf("the default registry's images passed for %s", short)
			}
		}
		if err := requireRegistryImages(writeCompose(t, "ghcr.io", "1"), "ghcr.io", "1"); err != nil {
			t.Errorf("error: %v", err)
		}
	})
	t.Run("a tag other than the version is refused", func(t *testing.T) {
		err := requireRegistryImages(writeCompose(t, reg, "dev"), reg, "test-42")
		if err == nil || !strings.Contains(err.Error(), "--build-arg VERSION=test-42") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a compose file without versola images cannot be checked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "c.yml")
		if err := os.WriteFile(path, []byte("services:\n  a:\n    image: postgres:18\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := requireRegistryImages(path, reg, "1"); err == nil {
			t.Error("no error")
		}
	})
	t.Run("a missing file is an error", func(t *testing.T) {
		if err := requireRegistryImages(filepath.Join(t.TempDir(), "nope.yml"), reg, "1"); err == nil {
			t.Error("no error")
		}
	})
}

func TestSplitImage(t *testing.T) {
	for in, want := range map[string][2]string{
		"ghcr.io/versolauth/versola-auth:0.7.2":    {"ghcr.io/versolauth/versola-auth", "0.7.2"},
		"localhost:5000/team/versola-auth:1":       {"localhost:5000/team/versola-auth", "1"},
		"localhost:5000/team/versola-auth":         {"localhost:5000/team/versola-auth", ""},
		"cr.yandex/x/versola-auth@sha256:abcdef01": {"cr.yandex/x/versola-auth", ""},
	} {
		repo, tag := splitImage(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("splitImage(%q) = %q, %q; want %q, %q", in, repo, tag, want[0], want[1])
		}
	}
}

func TestRegistryDeniedTexts(t *testing.T) {
	for _, stderr := range []string{
		"docker: Error response from daemon: Head \"https://cr.yandex/v2/crp/versola-tools/manifests/1\": unauthorized.",
		"Error response from daemon: pull access denied for x/versola-tools, repository does not exist or may require 'docker login'",
		"denied: requested access to the resource is denied",
		"Error: no basic auth credentials",
		"authentication required",
		"Error response from daemon: Head \"https://ghcr.io/v2/x/versola-tools/manifests/1\": denied",
	} {
		if !mentionsRegistryDenied(stderr) {
			t.Errorf("not recognized: %q", stderr)
		}
	}
	for _, stderr := range []string{
		"cp: can't create '/out/auth.conf': Permission denied",
		"manifest unknown",
		"",
	} {
		if mentionsRegistryDenied(stderr) {
			t.Errorf("wrongly recognized: %q", stderr)
		}
	}
}

func TestRegistryProblem(t *testing.T) {
	boom := errors.New("exit status 125")
	denied := &registryDeniedErr{inner: boom}
	unknown := &manifestUnknownErr{inner: boom}

	err := registryProblem(denied, "cr.yandex/crp/versola-tools:1", "cr.yandex/crp")
	if err == nil || !strings.Contains(err.Error(), "docker login cr.yandex") {
		t.Errorf("denied: %v", err)
	}
	// The default registry is logged in to by its host as well.
	if err := registryProblem(denied, "ghcr.io/versolauth/versola-tools:1", ""); err == nil || !strings.Contains(err.Error(), "docker login ghcr.io") {
		t.Errorf("denied, default: %v", err)
	}

	if err := registryProblem(unknown, "cr.yandex/crp/versola-tools:1", "cr.yandex/crp"); err == nil || !strings.Contains(err.Error(), "cr.yandex/crp/versola-tools:1") {
		t.Errorf("unknown, custom: %v", err)
	}
	// The default registry keeps its own message (the caller's).
	if err := registryProblem(unknown, "ghcr.io/versolauth/versola-tools:1", ""); err != nil {
		t.Errorf("unknown, default: %v", err)
	}
	if err := registryProblem(boom, "x", "cr.yandex/crp"); err != nil {
		t.Errorf("unrelated: %v", err)
	}
}
