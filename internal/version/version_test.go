package version

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenApiMatchesTheSpecification: the panel now tells an operator which contract this build
// answers, and a constant that says so is only worth having if it cannot drift from the document it
// describes. The failure it guards against is silent -- a sync endpoint changes, openapi.yaml's
// info.version is bumped with it, and the panel goes on reporting the previous contract to the one
// person who would act on the difference.
func TestOpenApiMatchesTheSpecification(t *testing.T) {
	specificationPath := filepath.Join("..", "..", "docs", "openapi.yaml")
	specification, err := os.Open(specificationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer specification.Close()

	// The version wanted is info.version, which is the document's own version and not one of the
	// many `version:` fields inside the schemas, so this stops at the first one after `info:`.
	var declared string
	insideInfo := false
	lines := bufio.NewScanner(specification)
	for lines.Scan() {
		line := lines.Text()
		if line == "info:" {
			insideInfo = true
			continue
		}
		if !insideInfo {
			continue
		}
		value, isVersion := strings.CutPrefix(line, "  version:")
		if isVersion {
			declared = strings.Trim(strings.TrimSpace(value), `"'`)
			break
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}

	if declared == "" {
		t.Fatalf("no info.version found in %s", specificationPath)
	}
	if declared != OpenApi {
		t.Fatalf("version.OpenApi = %s, but %s declares %s", OpenApi, specificationPath, declared)
	}
}
