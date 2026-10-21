package web

import (
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
}
