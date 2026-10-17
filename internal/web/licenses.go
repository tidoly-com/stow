package web

import (
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/tidoly-com/stow/internal/store"
)

// SourceURL is where the source of this program is offered to everyone who uses it, as the AGPL asks.
// The public repository is to be made apart from the one this is written in; change this line then.
const SourceURL = "https://github.com/tidoly-com/stow"

// licenses shows this program's license, where its source is, what else it carries and under which
// terms; the notices themselves are the files the installer put beside the program (LICENSES/).
func (w *Web) licenses(rw http.ResponseWriter, r *http.Request, user store.User) {
	data := w.frame(r.Context(), user, "오픈소스 라이선스", "licenses")
	type notice struct{ Path, Name string }
	var files, goFiles []notice
	if w.Licenses != "" {
		fs.WalkDir(os.DirFS(w.Licenses), ".", func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil || d.IsDir():
			case strings.HasPrefix(p, "go/"): // the Go modules' many notices, folded below the others
				goFiles = append(goFiles, notice{p, strings.TrimPrefix(p, "go/")})
			default:
				files = append(files, notice{p, p})
			}
			return nil
		})
	}
	data["Files"], data["GoFiles"], data["GoNotices"] = files, goFiles, w.Licenses != ""
	w.render(rw, "licenses", data)
}

// licenseFiles serves the notices in LICENSES/ as plain text.
func (w *Web) licenseFiles() http.Handler {
	if w.Licenses == "" {
		return http.NotFoundHandler()
	}
	files := http.StripPrefix("/licenses/files/", http.FileServerFS(os.DirFS(w.Licenses)))
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, ok := w.User(r); !ok {
			http.Redirect(rw, r, "/login", http.StatusSeeOther)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/") {
			rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		files.ServeHTTP(rw, r)
	})
}
