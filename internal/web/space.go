package web

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/tidoly-com/stow/internal/store"
)

// A Space is a second set of pages beside the photos, which a build may bring with it. With one, the menu
// starts with a switch between the photos and it, and shows the menu of the side the page is on; without,
// the menu is the photos' alone, as it has always been. The pages of a space are mounted by the build
// that has them (Mount), drawn in the same frame as the photos' (AddPages, Frame, Render).
type Space struct {
	Name string // on the switch
	Link string // where the switch leads
	Menu []MenuItem
}

// MenuItem is one line of a space's menu. Icon is the address of an SVG symbol; Tab is what a page of it
// passes to Frame, so that the line shows as the place one is.
type MenuItem struct{ Name, Icon, Link, Tab string }

// AddPages adds templates to those every page is drawn from, so that a space's own pages can use the
// photos' head, menu and foot. It must be called before the server starts.
func AddPages(fsys fs.FS, patterns ...string) {
	pages = template.Must(pages.ParseFS(fsys, patterns...))
}

// Page lets in only someone with a session, as the photos' pages do.
func (w *Web) Page(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return w.page(next)
}

// Frame is what every page needs around its own content; tab names the menu line that is lit.
func (w *Web) Frame(ctx context.Context, user store.User, title, tab string) map[string]any {
	return w.frame(ctx, user, title, tab)
}

// Render draws the named template.
func (w *Web) Render(rw http.ResponseWriter, name string, data any) { w.render(rw, name, data) }

// Fail answers with what went wrong in a way fit to show.
func (w *Web) Fail(rw http.ResponseWriter, err error) { w.fail(rw, err) }

// space is the space a page with this tab belongs to; nil for the photos.
func (w *Web) space(tab string) *Space {
	for i := range w.Spaces {
		for _, m := range w.Spaces[i].Menu {
			if m.Tab == tab {
				return &w.Spaces[i]
			}
		}
	}
	return nil
}
