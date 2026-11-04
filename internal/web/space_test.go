package web

import (
	"context"
	"strings"
	"testing"

	"github.com/tidoly-com/stow/internal/store"
)

// Without a second set of pages the menu is the photos' and nothing else; with one, a switch leads between
// the two, and a page of that set shows that set's menu in place of the photos'.
func TestASpaceBringsASwitchAndItsOwnMenu(t *testing.T) {
	nav := func(w *Web, tab string) string {
		data := map[string]any{"Tab": tab, "User": store.User{}, "Edition": w.Edition, "Spaces": w.Spaces}
		if s := w.space(tab); s != nil {
			data["Space"] = s
		}
		var b strings.Builder
		if err := pages.ExecuteTemplate(&b, "nav", data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	plain := nav(&Web{}, "timeline")
	if strings.Contains(plain, `class="spaces"`) || strings.Contains(plain, `class="edition"`) || !strings.Contains(plain, "인물") {
		t.Errorf("without a space:\n%s", plain)
	}
	w := &Web{Edition: "Plus", Spaces: []Space{{Name: "보관함", Link: "/notes", Menu: []MenuItem{{Name: "메모", Icon: "/x.svg#note", Link: "/notes", Tab: "notes"}}}}}
	photos := nav(w, "timeline")
	if !strings.Contains(photos, `class="spaces"`) || !strings.Contains(photos, "인물") || strings.Contains(photos, ">메모<") ||
		!strings.Contains(photos, `<a href="/" aria-current="page">사진</a>`) || !strings.Contains(photos, `<span class="edition">Plus</span>`) {
		t.Errorf("a photos page with a space beside:\n%s", photos)
	}
	notes := nav(w, "notes")
	if strings.Contains(notes, "인물") || !strings.Contains(notes, `<a href="/notes" aria-current="page">보관함</a>`) || !strings.Contains(notes, "메모</a>") || !strings.Contains(notes, "휴지통") {
		t.Errorf("a page of the space:\n%s", notes)
	}
	// a page of the space may put the pieces of what is open under its line
	w.Spaces[0].Menu = append(w.Spaces[0].Menu, MenuItem{Name: "허브", Icon: "/x.svg#hub", Link: "/hubs", Tab: "hubs"})
	data := map[string]any{"Tab": "hubs", "User": store.User{}, "Edition": w.Edition, "Spaces": w.Spaces, "Space": w.space("hubs"),
		"SideSub": []SubItem{{Name: "개발", Link: "/hubs/a", Current: true}, {Name: "#23", Link: "/hubs/b", Inner: true}, {Name: "1기", Link: "/hubs/c", Inner: true, Deeper: true}}}
	var b strings.Builder
	if err := pages.ExecuteTemplate(&b, "nav", data); err != nil {
		t.Fatal(err)
	}
	if hubs := b.String(); !strings.Contains(hubs, `<div class="side-sub"><a href="/hubs/a" aria-current="page" title="개발"><span>개발</span></a><a href="/hubs/b" class="inner" title="#23"><span>#23</span></a><a href="/hubs/c" class="inner deeper" title="1기"><span>1기</span></a></div>`) ||
		strings.Count(hubs, "side-sub") != 1 {
		t.Errorf("the pieces under the open line:\n%s", hubs)
	}
}

// A space that keeps a trash of its own: 휴지통 on its side leads there, the page is drawn in its menu, and
// the trash shows the kinds as tabs, the photos' first.
func TestASpacesTrashIsATabBesideThePhotos(t *testing.T) {
	w := &Web{Edition: "Plus", Spaces: []Space{{Name: "보관함", Link: "/notes", Trash: "/docs/trash",
		Menu: []MenuItem{{Name: "메모", Icon: "/x.svg#note", Link: "/notes", Tab: "notes"}}}},
		TrashTabs: []TrashTab{{Name: "문서", Link: "/docs/trash", Count: func(context.Context, store.User) int { return 3 }}}}
	draw := func(name string, data any) string {
		var b strings.Builder
		if err := pages.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	notes := draw("nav", map[string]any{"Tab": "notes", "User": store.User{}, "Spaces": w.Spaces, "Space": w.space("notes")})
	if !strings.Contains(notes, `<a href="/docs/trash">`) {
		t.Errorf("휴지통 from the space's side:\n%s", notes)
	}
	photos := draw("nav", map[string]any{"Tab": "timeline", "User": store.User{}, "Spaces": w.Spaces})
	if !strings.Contains(photos, `<a href="/trash">`) {
		t.Errorf("휴지통 from the photos' side:\n%s", photos)
	}
	data := map[string]any{"Tab": "trash", "User": store.User{}, "Spaces": w.Spaces}
	w.InSpace(data, "보관함")
	if !strings.Contains(draw("nav", data), `<a href="/docs/trash" aria-current="page">`) {
		t.Error("the space's trash is not drawn in its menu")
	}
	tabs := draw("trash-tabs", w.TrashTabsFor(context.Background(), store.User{}, "/docs/trash"))
	if !strings.Contains(tabs, `<a href="/trash">사진 <small>0</small></a><a href="/docs/trash" aria-current="page">문서 <small>3</small></a>`) {
		t.Errorf("tabs:\n%s", tabs)
	}
	if (&Web{}).TrashTabsFor(context.Background(), store.User{}, "/trash") != nil {
		t.Error("tabs without a space's trash")
	}
}
