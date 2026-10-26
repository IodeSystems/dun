package main

import (
	"testing"

	"github.com/iodesystems/dun"
	dunreg "github.com/iodesystems/dun/internal/registry"
	"github.com/iodesystems/dun/release"
)

// The registry's pin raises a tool's Min and Recommended for doctor, and
// never lowers them.
func TestSupportedTool_RegistryRaises(t *testing.T) {
	built, _ := release.SupportedTool("raglit")
	ix, err := dunreg.Parse([]byte(`{"schema": 1, "published": "2026-10-26T10:00:00Z", "tools": [
	  {"id": "raglit", "version": "1.0.0", "dun": "*", "min": "v0.0.0-20200101000000-000000000000", "recommended": "v9.9.9"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	dun.UseRegistry(ix, "v0.1.3")
	t.Cleanup(func() { dun.UseRegistry(nil, "") })
	s, ok := supportedTool("raglit")
	if !ok || s.Recommended != "v9.9.9" || s.Min != built.Min {
		t.Errorf("raglit: %+v (built %+v)", s, built)
	}
	if got := installModule("raglit"); got != built.Module+"@v9.9.9" {
		t.Errorf("installModule: %s", got)
	}
}
