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
	Name  string // on the switch
	Link  string // where the switch leads
	Menu  []MenuItem
	Trash string // where 휴지통 leads from its side, "" for the photos' trash
	// Start names the space's own first step, which a build without photos takes in theirs: drawn from its
	// "start-extra" template at /start/space. "" for none
	Start string
}

// A TrashTab is one kind of thing a space keeps in the trash, as a tab over the trash beside the photos'
// (사진 · 문서 · 메모 · 허브): one trash in the menu, the kinds side by side.
type TrashTab struct {
	Name, Link string
	Count      func(context.Context, store.User) int
}

// TrashTabLine is a tab as the page draws it.
type TrashTabLine struct {
	Name, Link string
	Count      int
	Current    bool
}

// TrashTabsFor are the trash's tabs — the photos' first, where there are photos — with the one at current marked; none when no
// space keeps a trash of its own.
func (w *Web) TrashTabsFor(ctx context.Context, user store.User, current string) []TrashTabLine {
	if len(w.TrashTabs) == 0 {
		return nil
	}
	var lines []TrashTabLine
	if !w.Alone { // a build without photos has no trash of photos
		photos := 0
		if w.Store != nil {
			if assets, err := w.Store.TrashedAssets(ctx, user.ID, trashLimit); err == nil {
				photos = len(assets)
			}
		}
		lines = append(lines, TrashTabLine{Name: "사진", Link: "/trash", Count: photos, Current: current == "/trash"})
	}
	for _, t := range w.TrashTabs {
		n := 0
		if t.Count != nil {
			n = t.Count(ctx, user)
		}
		lines = append(lines, TrashTabLine{Name: t.Name, Link: t.Link, Count: n, Current: t.Link == current})
	}
	return lines
}

// InSpace draws a page in the menu of the space named, though no line of its menu is the page's own — the
// trash of a space, reached from its side.
func (w *Web) InSpace(data map[string]any, name string) {
	for i := range w.Spaces {
		if w.Spaces[i].Name == name {
			data["Space"] = &w.Spaces[i]
			return
		}
	}
}

// MenuItem is one line of a space's menu. Icon is the address of an SVG symbol; Tab is what a page of it
// passes to Frame, so that the line shows as the place one is.
type MenuItem struct{ Name, Icon, Link, Tab string }

// SubItem is one line a page of a space puts under its own line of the menu, as SideSub: the pieces of
// what is open, indented. Inner lines are one step further in, Deeper ones two; the Current one is the page
// one is on.
type SubItem struct {
	Name, Link             string
	Inner, Deeper, Current bool
}

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
