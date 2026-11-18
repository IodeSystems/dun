package memory

import (
	"strings"
	"testing"
)

func TestLearn_GenericityFilter(t *testing.T) {
	names := map[string]bool{"render_scene": true, "render_scene.py": true, "src": true, "makefile": true}
	task := "Build POV-Ray 2.2 and render `illum1.pov`. The scene must match the reference image exactly."
	episode := "python3 -c 'import parseMoves; parseMoves.run()'\nTraceback: frame_buffer overflow"
	ok := []string{
		"When a compiler or build tool is not found, refresh the system package index and install it before retrying the build.",
		"Run the module with python3 -m pip when pip is not on the path. Check the makefile targets before guessing one.",
		"Set LD_LIBRARY_PATH when a built library is not found at run time.",
	}
	for _, d := range ok {
		if err := genericityError(lesson{Name: "x-y", Detail: d}, task, episode, names); err != nil {
			t.Errorf("rejected %q: %v", d, err)
		}
	}
	bad := map[string]string{
		"Edit /app/povray-2.2/source/config.h before running make.":                    "path",
		"Run make in src/unix/build first.":                                            "path",
		"Fix the import in render.py.":                                                 "file name",
		"Download it from https://example.com/x.tgz.":                                  "URL",
		"Check out commit 3f9a2c1d before building.":                                   "hash",
		"Call render_scene with the scene file.":                                       "workspace",
		"Build POV-Ray from its archive.":                                              "task",
		"The parseMoves module needs its run function.":                                "failed calls",
		"Grow frame_buffer before the second pass.":                                    "failed calls",
		"The scene must match the reference image exactly, so diff the output.":        "repeats the task",
		"Use \"for (int i = 0; i < n; i++) { buf[i] = 0; } return buf;\" to clear it.": "quotes",
		"One. Two. Three. Four.":                                                       "sentences",
		"Use ```make``` here.":                                                         "code",
	}
	for d, want := range bad {
		err := genericityError(lesson{Name: "x-y", Detail: d}, task, episode, names)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %s", d, err, want)
		}
	}
}
