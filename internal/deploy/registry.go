package deploy

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// DefaultRegistry is where Versola's own releases are published: the prefix
// of versola-tools and of the central, auth and edge images that
// versola-tools' compose file names.
const DefaultRegistry = "ghcr.io/versolauth"

// registryPattern is a registry host (optionally with a port) followed by any
// number of repository path components, in the lower-case form Docker
// requires for the path.
var registryPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// ParseRegistry validates --registry: the prefix the Versola images are named
// under, like "cr.yandex/crp1a2b3c" (images are then
// cr.yandex/crp1a2b3c/versola-tools:<version>, and so on). It returns "" for
// no override -- an empty value, or the default registry spelled out -- which
// is also how a deployment from the default registry is recorded.
func ParseRegistry(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == DefaultRegistry {
		return "", nil
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("--registry is a registry host and path without a scheme (e.g. cr.yandex/crp1a2b3c), not %q", s)
	}
	if strings.HasSuffix(s, "/") {
		return "", fmt.Errorf("--registry has no trailing slash (e.g. cr.yandex/crp1a2b3c), not %q", s)
	}
	if !registryPattern.MatchString(s) {
		return "", fmt.Errorf("--registry %q is not a registry host and path (lower-case, e.g. cr.yandex/crp1a2b3c or registry.example.com:5000/versola)", s)
	}
	return s, nil
}

// registryOrDefault is the prefix to use for a recorded registry, which is ""
// for the default.
func registryOrDefault(registry string) string {
	if registry == "" {
		return DefaultRegistry
	}
	return registry
}

// registryHost is the part of a registry prefix `docker login` takes.
func registryHost(registry string) string {
	host, _, _ := strings.Cut(registryOrDefault(registry), "/")
	return host
}

// requireRegistryImages checks, in the compose file versola-tools just wrote,
// that it really names the Versola images under registry, at this version.
// Without it, a versola-tools release that does not know IMAGE_REGISTRY would
// ignore it and the stack would come up from the default registry: the tools
// image would be the one that was asked for, and central, auth and edge would
// be somebody else's. Nothing to check for the default registry (registry is
// "").
//
// The tag is checked as well: the compose file takes it from the VERSION baked
// into the tools image, not from the tag the CLI pulled, so an image that was
// pushed under another tag than it was built with would deploy another build
// than the one asked for.
func requireRegistryImages(composeFile, registry, version string) error {
	if registry == "" {
		return nil
	}
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return fmt.Errorf("couldn't read the generated compose file: %w", err)
	}
	images := versolaImagesIn(string(data))
	if len(images) == 0 {
		return fmt.Errorf("the compose file versola-tools generated names no versola-* images, so --registry %s can't be checked -- this versola-tools release is not one this versola-cli can deploy from another registry", registry)
	}
	for _, image := range images {
		repo, tag := splitImage(image)
		// Exactly registry/versola-<name>: a deeper path is another registry's
		// (--registry ghcr.io must not accept ghcr.io/versolauth/versola-auth).
		name, under := strings.CutPrefix(repo, registry+"/")
		if !under || strings.Contains(name, "/") {
			return fmt.Errorf("this versola-tools release ignores --registry: its compose file still names %s, not an image under %s. Build and publish versola-tools from a Versola revision that supports IMAGE_REGISTRY (nothing was started)", image, registry)
		}
		if tag != version {
			return fmt.Errorf("the compose file names %s, but %s was asked for: versola-tools takes the images' tag from the VERSION it was built with, so build it with --build-arg VERSION=%s (nothing was started)", image, version, version)
		}
	}
	return nil
}

var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*["']?([^\s"']+)`)

// versolaImagesIn lists the image references of Versola's own services
// (repositories named versola-*) in a compose file's text.
func versolaImagesIn(compose string) []string {
	var out []string
	for _, m := range imageLine.FindAllStringSubmatch(compose, -1) {
		repo, _ := splitImage(m[1])
		if strings.HasPrefix(repo[strings.LastIndex(repo, "/")+1:], "versola-") {
			out = append(out, m[1])
		}
	}
	return out
}

// splitImage splits "host:5000/path/name:tag" into the repository and the
// tag; a digest reference has no tag here ("" -- it never equals a version).
func splitImage(image string) (repo, tag string) {
	if i := strings.Index(image, "@"); i >= 0 {
		return image[:i], ""
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, ""
}

// registryProblem turns a failed versola-tools run into the message a
// registry other than the default calls for, or returns nil when err has
// nothing to do with the registry (the caller then reports it as before).
//
//   - A refused login: this CLI never logs in to a registry, `docker login`
//     is the operator's.
//   - A missing image in a registry that is not the default: the usual
//     message names GitHub's package list, which is the wrong place to look.
func registryProblem(err error, image, registry string) error {
	switch {
	case isRegistryDenied(err):
		hint := ""
		if registry == "" {
			// GHCR answers "denied" for a public image when a stale or expired
			// token for it is stored.
			hint = " (these images are public: if a login for it is stored and expired, `docker logout ghcr.io` instead)"
		}
		return fmt.Errorf("the registry refused to give out %s: it needs a login this CLI doesn't do -- run `docker login %s` first, and check the account may pull from there%s", image, registryHost(registry), hint)
	case registry != "" && isManifestUnknown(err):
		return fmt.Errorf("%s doesn't exist (the registry has no such image or tag, or the account can't see it) -- versola-tools has to be pushed under the version you pass, to %s/versola-tools", image, registry)
	}
	return nil
}
