package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// A build without photos (Alone, #47): its space is the whole menu, on its own pages and on the admin page;
// there is no switch, no photo page answers, the admin page has no photos' parts, the trash has no photos'
// tab, and the first steps take the space's own in place of the photos'.
func TestABuildWithoutPhotos(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner, err := st.UpsertUser(ctx, "local:kim@example.com", "kim@example.com", "김", true)
	if err != nil {
		t.Fatal(err)
	}
	w := &Web{Store: st, Dir: t.TempDir(), Address: "http://192.168.0.10:30991", Edition: "Plus", Alone: true,
		User:     func(*http.Request) (store.User, bool) { return owner, true },
		Outside:  func(context.Context) Outside { return Outside{} },
		Arrivals: func(context.Context) ([]store.User, error) { return nil, nil },
		Spaces: []Space{{Name: "보관함", Link: "/docs", Trash: "/docs/trash", Start: "기존 문서",
			Menu: []MenuItem{{Name: "문서", Icon: "/x.svg#doc", Link: "/docs", Tab: "docs"}}}},
		TrashTabs: []TrashTab{{Name: "문서", Link: "/docs/trash"}},
		MeMenu:    []MenuItem{{Name: "AI 연결", Link: "/ai", Tab: "ai"}}}
	noop := func(http.ResponseWriter, *http.Request) {}
	srv := httptest.NewServer(w.Handler(noop, noop))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		res, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	if res, _ := get("/"); res.StatusCode != 303 || res.Header.Get("Location") != "/docs" {
		t.Errorf("/: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := get("/trash"); res.Header.Get("Location") != "/docs/trash" {
		t.Errorf("/trash: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, path := range []string{"/people", "/albums", "/places", "/links", "/status", "/start/folders", "/start/auto", "/phone"} {
		if res, _ := get(path); res.StatusCode != 404 {
			t.Errorf("%s answers %d without photos", path, res.StatusCode)
		}
	}
	_, admin := get("/admin")
	for _, gone := range []string{`class="spaces"`, `data-browse aria-label="위치 더하기"`, "인물", "기존 사진 폴더", "자동 앨범", "얼굴인식", "폰 연결 · 상태", "공유 링크", "사진 · 영상", "Immich"} {
		if strings.Contains(admin, gone) {
			t.Errorf("the admin page without photos shows %q", gone)
		}
	}
	for _, want := range []string{`<a href="/docs"><svg class="i" aria-hidden="true"><use href="/x.svg#doc"/></svg>문서</a>`, `<a href="/docs/trash">`, "문서와 설정파일이 저장되는 경로입니다.", "새 문서 저장 위치", "자기 문서를 따로 가집니다", "연결 설정", `<a href="/ai">AI 연결</a><a href="/help">도움말</a>`} {
		if !strings.Contains(admin, want) {
			t.Errorf("the admin page without photos has no %q", want)
		}
	}
	if tabs := w.TrashTabsFor(ctx, owner, "/docs/trash"); len(tabs) != 1 || tabs[0].Name != "문서" {
		t.Errorf("trash tabs: %+v", tabs)
	}
	if got := strings.Join(w.stepNames(), " "); got != "저장 위치 준비 첫 계정 화면 기존 문서 연결 설정 다른 기기" {
		t.Errorf("first steps: %s", got)
	}
	if _, screen := get("/start/screen"); !strings.Contains(screen, `<a class="button" href="/start/space">다음: 기존 문서</a>`) {
		t.Errorf("the screen step does not lead to the space's own:\n%s", screen)
	}
	if res, page := get("/start/space"); res.StatusCode != 200 || !strings.Contains(page, `<a class="button" href="/outside">다음: 연결 설정</a>`) {
		t.Errorf("the space's own step: %d\n%s", res.StatusCode, page)
	}
	if _, last := get("/welcome"); !strings.Contains(last, "다른 기기에서 열기") || strings.Contains(last, "Immich") || !strings.Contains(last, `<a class="button" href="/">설정 완료</a>`) {
		t.Errorf("the last step:\n%s", last)
	}
	if text := w.invite(ctx, "lee@example.com"); strings.Contains(text, "Immich") || !strings.Contains(text, "lee@example.com") || !strings.Contains(text, "http://192.168.0.10:30991") {
		t.Errorf("invite: %s", text)
	}
}
