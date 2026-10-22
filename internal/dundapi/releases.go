package dundapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Android releases, as bintag serves them: tools/release-android writes
// <releases>/android/dun-<code>.apk and android/latest.json; dund serves
// the directory under /releases, /dun.apk redirects to the latest, and
// /api/v1/hello announces it so the app can offer the update.

const (
	releasesPath = "/releases"
	apkPath      = "/dun.apk"
	apkMIME      = "application/vnd.android.package-archive"
)

// AndroidRelease is the latest APK, from android/latest.json.
type AndroidRelease struct {
	VersionCode int64  `json:"version_code" doc:"0 when no APK has been released"`
	VersionName string `json:"version_name,omitempty"`
	APKPath     string `json:"apk_path,omitempty" doc:"path of the APK on this server"`
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Notes       string `json:"notes,omitempty"`
	ReleasedAt  string `json:"released_at,omitempty"`
}

func (s *Server) latestRelease() AndroidRelease {
	if s.releases == "" {
		return AndroidRelease{}
	}
	b, err := os.ReadFile(filepath.Join(s.releases, "android", "latest.json"))
	if err != nil {
		return AndroidRelease{}
	}
	var m struct {
		VersionCode int64  `json:"version_code"`
		VersionName string `json:"version_name"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		File        string `json:"file"`
		Notes       string `json:"notes"`
		ReleasedAt  string `json:"released_at"`
	}
	if json.Unmarshal(b, &m) != nil || m.File == "" || strings.ContainsAny(m.File, "/\\") {
		return AndroidRelease{}
	}
	return AndroidRelease{VersionCode: m.VersionCode, VersionName: m.VersionName,
		APKPath: releasesPath + "/android/" + m.File, SHA256: m.SHA256, Size: m.Size,
		Notes: m.Notes, ReleasedAt: m.ReleasedAt}
}

func (s *Server) latestAPK(w http.ResponseWriter, r *http.Request) {
	rel := s.latestRelease()
	if rel.APKPath == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, rel.APKPath, http.StatusFound)
}

// releaseFiles serves the releases directory: files only, APKs with the type
// the package installer expects.
func releaseFiles(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no listings
			return
		}
		if strings.HasSuffix(r.URL.Path, ".apk") {
			w.Header().Set("Content-Type", apkMIME)
		}
		files.ServeHTTP(w, r)
	})
}
