package immich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tidoly-com/stow/internal/media/mediatest"
)

// post sends a form as a browser does, with its cookies; redirects are not followed.
func (e *env) post(path string, form url.Values, cookies ...*http.Cookie) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	res, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// A link shows only its owner's photos to someone without an account, and only while the tunnel is on;
// it ends, takes a password, and keeps originals back when told to. The pages make one for seven days.
func TestSharedLinks(t *testing.T) {
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	phone := e.login("owner@example.com", "owner")
	kid := e.login("kid@example.com", "member")
	_, a := e.upload(phone, "a.jpg", mediatest.JPEG(nil, 40, 30))
	_, b := e.upload(phone, "b.jpg", mediatest.JPEG(nil, 42, 30))
	_, k := e.upload(kid, "k.jpg", mediatest.JPEG(nil, 44, 30))
	e.settle()
	aID, bID, kID := a["id"].(string), b["id"].(string), k["id"].(string)

	// no tunnel, no links: the app is told they are off
	if status, _ := e.call("POST", "/api/shared-links", phone, map[string]any{"type": "INDIVIDUAL", "assetIds": []string{aID}}); status != 404 {
		t.Fatalf("without the tunnel: %d", status)
	}
	cookie := e.browserLogin("owner@example.com", "owner") // before the address outside: the tests reach the server here
	host := testSlug + ".stow.tidoly.app"
	if err := e.api.setLink(ctx, Link{Slug: testSlug, Host: host, Key: "tdly_stow_secret"}); err != nil {
		t.Fatal(err)
	}
	var prefs map[string]any
	_, raw := e.call("GET", "/api/users/me/preferences", phone, nil)
	json.Unmarshal(raw, &prefs)
	if on, _ := prefs["sharedLinks"].(map[string]any)["enabled"].(bool); !on {
		t.Errorf("preferences: %s", raw)
	}

	// the app's "share link" of a choice: only the owner's own photo is taken
	status, raw := e.call("POST", "/api/shared-links", phone, map[string]any{"type": "INDIVIDUAL", "assetIds": []string{aID, kID}})
	var link map[string]any
	json.Unmarshal(raw, &link)
	key, id := link["key"].(string), link["id"].(string)
	if status != 201 || key == "" || len(link["assets"].([]any)) != 1 || link["allowDownload"] != true || link["showMetadata"] != true || link["password"] != nil {
		t.Fatalf("create: %d %s", status, raw)
	}
	thumb := func(asset, size string, cookies ...*http.Cookie) int {
		res, _ := e.browse("/api/assets/"+asset+"/"+size+"?key="+key, cookies...)
		return res.StatusCode
	}
	if thumb(aID, "thumbnail") != 200 || thumb(aID, "original") != 200 || thumb(bID, "thumbnail") != 404 || thumb(kID, "thumbnail") != 404 {
		t.Errorf("files through the key: a %d %d, b %d, kid's %d", thumb(aID, "thumbnail"), thumb(aID, "original"), thumb(bID, "thumbnail"), thumb(kID, "thumbnail"))
	}
	res, page := e.browse("/share/" + key)
	if res.StatusCode != 200 || !strings.Contains(page, "/api/assets/"+aID+"/thumbnail?key="+key) || strings.Contains(page, bID) ||
		!strings.Contains(page, "tidoly stow</b> 로 공유됨") || res.Header.Get("X-Robots-Tag") == "" {
		t.Fatalf("page: %d %s", res.StatusCode, page)
	}
	if res, one := e.browse("/share/" + key + "/" + aID); res.StatusCode != 200 || !strings.Contains(one, "/original?key="+key) {
		t.Errorf("one photo: %d %s", res.StatusCode, one)
	}

	// originals kept back; a password; then ended
	if status, raw := e.call("PATCH", "/api/shared-links/"+id, phone, map[string]any{"allowDownload": false, "password": "1234"}); status != 200 || !strings.Contains(string(raw), `"password":"********"`) {
		t.Fatalf("edit: %d %s", status, raw)
	}
	if res, page := e.browse("/share/" + key); res.StatusCode != 200 || !strings.Contains(page, `name="password"`) || strings.Contains(page, aID) {
		t.Fatalf("with a password: %d %s", res.StatusCode, page)
	}
	if thumb(aID, "thumbnail") != 404 {
		t.Error("a photo is given without the password")
	}
	if res := e.post("/share/"+key, url.Values{"password": {"wrong"}}); res.StatusCode != 403 {
		t.Errorf("wrong password: %d", res.StatusCode)
	}
	res = e.post("/share/"+key, url.Values{"password": {"1234"}})
	if res.StatusCode != 303 || len(res.Cookies()) != 1 {
		t.Fatalf("right password: %d", res.StatusCode)
	}
	open := res.Cookies()[0]
	if thumb(aID, "thumbnail", open) != 200 || thumb(aID, "original", open) != 404 {
		t.Errorf("after the password: thumbnail %d, original %d", thumb(aID, "thumbnail", open), thumb(aID, "original", open))
	}
	// the app sends the shown password back unchanged: it stays
	e.call("PATCH", "/api/shared-links/"+id, phone, map[string]any{"password": passwordShown, "description": "가을"})
	if thumb(aID, "thumbnail", open) != 200 {
		t.Error("the password changed when it was sent back as shown")
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	e.call("PATCH", "/api/shared-links/"+id, phone, map[string]any{"expiresAt": past})
	if res, _ := e.browse("/share/" + key); res.StatusCode != 404 || thumb(aID, "thumbnail", open) != 404 {
		t.Errorf("ended: page %d", res.StatusCode)
	}

	// an album's link shows the album as it is; another person's album cannot be shared
	_, raw = e.call("POST", "/api/albums", phone, map[string]any{"albumName": "여행", "assetIds": []string{bID}})
	var album map[string]any
	json.Unmarshal(raw, &album)
	if status, _ := e.call("POST", "/api/shared-links", kid, map[string]any{"type": "ALBUM", "albumId": album["id"]}); status != 404 {
		t.Errorf("someone else's album: %d", status)
	}
	status, raw = e.call("POST", "/api/shared-links", phone, map[string]any{"type": "ALBUM", "albumId": album["id"]})
	json.Unmarshal(raw, &link)
	albumKey := link["key"].(string)
	if res, page := e.browse("/share/" + albumKey); status != 201 || res.StatusCode != 200 || !strings.Contains(page, bID) || !strings.Contains(page, "여행") {
		t.Fatalf("album link: %d %d", status, res.StatusCode)
	}
	if status, raw := e.call("GET", "/api/shared-links", phone, nil); status != 200 || strings.Count(string(raw), `"key"`) != 2 {
		t.Errorf("list: %d %s", status, raw)
	}

	// the pages make one for seven days, and list it
	req, _ := http.NewRequest("POST", e.server.URL+"/web/links", strings.NewReader(url.Values{"id": {aID}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var made map[string]string
	json.NewDecoder(res.Body).Decode(&made)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(made["url"], "https://"+host+"/share/") {
		t.Fatalf("made on the pages: %d %v", res.StatusCode, made)
	}
	l, err := e.store.SharedLink(ctx, e.userID("owner@example.com"), made["id"])
	if until, _ := time.Parse(time.RFC3339, l.ExpiresAt); err != nil || until.Sub(time.Now()) < 6*24*time.Hour || until.Sub(time.Now()) > 8*24*time.Hour {
		t.Errorf("made for %v", l.ExpiresAt)
	}
	if _, page := e.browse("/links", cookie); !strings.Contains(page, made["url"]) || !strings.Contains(page, "여행") {
		t.Errorf("the list: %s", page)
	}

	// ended by its owner; and no link opens once the tunnel is off
	if status, _ := e.call("DELETE", "/api/shared-links/"+id, phone, nil); status != 204 {
		t.Errorf("delete: %d", status)
	}
	if err := e.api.setLink(ctx, Link{Slug: testSlug}); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.browse("/share/" + albumKey); res.StatusCode != 404 {
		t.Errorf("without the tunnel the page opens: %d", res.StatusCode)
	}
}
