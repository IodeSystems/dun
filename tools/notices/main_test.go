package main

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	for text, want := range map[string]string{
		"Permission is hereby granted,\nfree of charge, to any person":      "MIT",
		"Apache License\n Version 2.0, January 2004":                        "Apache-2.0",
		"GNU GENERAL PUBLIC LICENSE Version 3":                              "GPL",
		"GNU AFFERO GENERAL PUBLIC LICENSE":                                 "AGPL",
		"Mozilla Public License Version 2.0":                                "MPL-2.0",
		"Redistributions in binary form must reproduce the above copyright": "BSD-2-Clause",
		"something else entirely":                                           "",
	} {
		if got := classify(text); got != want {
			t.Errorf("classify(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestNpmAllowed(t *testing.T) {
	for expr, want := range map[string]bool{
		"MIT":                       true,
		"(MIT OR Apache-2.0)":       true,
		"(BSD-3-Clause OR GPL-2.0)": true,
		"MIT AND GPL-3.0":           false,
		"MPL-2.0":                   false,
		"GPL-3.0":                   false,
		"":                          false,
		"SEE LICENSE IN LICENSE":    false,
	} {
		if got := npmAllowed(expr); got != want {
			t.Errorf("npmAllowed(%q) = %v, want %v", expr, got, want)
		}
	}
}

func TestParseLock(t *testing.T) {
	lock := `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@scope/a':
        specifier: ^1.0.0
        version: 1.2.3(react@19.0.0)
      b:
        specifier: ^2.0.0
        version: 2.0.0
    devDependencies:
      dev:
        specifier: ^1
        version: 1.0.0

packages:

  b@2.0.0:
    resolution: {integrity: x}

snapshots:

  '@scope/a@1.2.3(react@19.0.0)':
    dependencies:
      c: 3.0.0
      alias: npm:real@4.0.0
    optionalDependencies:
      d: 5.0.0

  b@2.0.0: {}
`
	roots, snaps := parseLock(lock)
	if want := []string{"@scope/a@1.2.3(react@19.0.0)", "b@2.0.0"}; !reflect.DeepEqual(roots, want) {
		t.Errorf("roots = %v, want %v", roots, want)
	}
	if got, want := snaps["@scope/a@1.2.3(react@19.0.0)"].deps, []string{"c@3.0.0", "real@4.0.0", "d@5.0.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
	if n, v := keyName("@scope/a@1.2.3(react@19.0.0)"); n != "@scope/a" || v != "1.2.3" {
		t.Errorf("keyName = %q %q", n, v)
	}
}
