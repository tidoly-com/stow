(function () {
  "use strict";
  const post = (path, fields) => {
    const body = new URLSearchParams();
    for (const [name, value] of Object.entries(fields)) [].concat(value).forEach((v) => body.append(name, v));
    return fetch(path, { method: "POST", body, credentials: "same-origin" }).then((r) => (r.ok ? r : Promise.reject(r.status)));
  };
  const toast = document.querySelector(".toast");
  let toastTimer;
  const say = (text) => {
    if (!toast) return;
    toast.textContent = text;
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 2600);
  };
  const failed = () => say("하지 못했습니다. 잠시 뒤에 다시 해 주세요.");

  // Rows that fill the width: photos are put in a row until it is full, then the row's height is set so
  // that they fit exactly, each keeping its shape. The last row of a group keeps the wished height. A
  // photo whose shape is not known yet counts as square.
  const picked = new Set();
  const shapeOf = (cell) => (cell.dataset.w ? Math.min(2.5, Math.max(0.5, cell.dataset.w / cell.dataset.h)) : 1);
  const lay = (flow) => {
    const style = getComputedStyle(flow), wish = parseFloat(style.getPropertyValue("--row")) || 200, gap = parseFloat(style.columnGap) || 0;
    const width = flow.clientWidth;
    if (!width) return;
    const set = (row, height) => row.forEach((cell) => { cell.style.height = height + "px"; cell.style.width = Math.floor(shapeOf(cell) * height * 100) / 100 + "px"; });
    const fit = (row) => (width - gap * (row.length - 1) - 0.5) / row.reduce((sum, cell) => sum + shapeOf(cell), 0);
    let row = [];
    for (const cell of flow.children) {
      row.push(cell);
      const height = fit(row);
      if (height > wish) continue; // not full yet
      if (row.length > 1 && wish - height > fit(row.slice(0, -1)) - wish) { // nearer the wish without the last one
        set(row.slice(0, -1), Math.min(fit(row.slice(0, -1)), wish * 1.5));
        row = [cell];
      } else {
        set(row, height);
        row = [];
      }
    }
    set(row, Math.min(wish, fit(row.length ? row : [flow])));
  };
  const dress = (root) => {
    root.querySelectorAll("a[data-photo]").forEach((cell) => cell.classList.toggle("picked", picked.has(cell.dataset.photo)));
    root.querySelectorAll(".flow").forEach(lay);
  };
  dress(document);
  // Admin: the person-plus beside "사용자" opens an empty row to fill in and save.
  const newUser = document.querySelector(".users .new");
  document.querySelectorAll('[data-act="add-user"]').forEach((b) => b.addEventListener("click", () => {
    newUser.hidden = false;
    newUser.querySelector("input").focus();
  }));
  document.querySelectorAll('[data-act="cancel-user"]').forEach((b) => b.addEventListener("click", () => {
    newUser.reset();
    newUser.hidden = true;
  }));

  // A folded group has no width to lay its photos out in until it is opened.
  document.querySelectorAll("details").forEach((d) => d.addEventListener("toggle", () => d.querySelectorAll(".flow").forEach(lay)));

  // Places: the slider picks how wide a place is — a country, a state, a city. The last choice is kept in
  // this browser; without it the page opens on countries.
  const widths = document.querySelector(".places .zoom input");
  if (widths) {
    const choose = () => {
      document.querySelectorAll(".places .width").forEach((w) => { w.hidden = w.dataset.width !== widths.value; });
      document.querySelectorAll(".places .stops span").forEach((s) => s.classList.toggle("on", s.dataset.width === widths.value));
      try { localStorage.setItem("places-width", widths.value); } catch (e) { /* kept for this page only */ }
    };
    try { widths.value = localStorage.getItem("places-width") || widths.value; } catch (e) { /* opens on countries */ }
    widths.addEventListener("input", choose);
    choose();
  }
  let relay;
  addEventListener("resize", () => { clearTimeout(relay); relay = setTimeout(() => document.querySelectorAll(".flow").forEach(lay), 80); });

  // The timeline has one section per month. Only the months near the screen hold pictures; the others
  // are kept at about the height their pictures would take, so the scroll bar is honest from the start.
  const months = Array.from(document.querySelectorAll("section[data-month]"));
  if (months.length) {
    const main = document.querySelector("main");
    const guess = (section) => {
      const style = getComputedStyle(main), row = parseFloat(style.getPropertyValue("--row")) || 200;
      const width = main.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
      return Math.ceil((section.dataset.count * row * 1.25) / width) * (row + 4) + 56;
    };
    const reserve = () => months.forEach((s) => { if (!s.dataset.loaded) s.style.minHeight = guess(s) + "px"; });
    reserve();
    addEventListener("resize", reserve);

    const near = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        const section = entry.target, box = section.querySelector(".days");
        if (entry.isIntersecting && !section.dataset.loaded) {
          section.dataset.loaded = "1";
          fetch("/web/month/" + section.dataset.month + (main.dataset.query || location.search), { credentials: "same-origin" })
            .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
            .then((html) => { if (section.dataset.loaded) { box.innerHTML = html; dress(box); section.style.minHeight = ""; } })
            .catch(() => { delete section.dataset.loaded; });
        } else if (!entry.isIntersecting && section.dataset.loaded) {
          section.style.minHeight = section.offsetHeight + "px"; // keep the place, drop the pictures
          box.innerHTML = "";
          delete section.dataset.loaded;
        }
      }
    }, { rootMargin: "1500px 0px" }); // a few screens ahead, so fast scrolling does not show blanks
    months.forEach((s) => near.observe(s));

    // The years down the right edge, each as far down as the share of photos newer than it. Pressing
    // one goes there; the line shows where the screen is now.
    const rail = document.querySelector(".years");
    if (rail) {
      const total = months.reduce((n, s) => n + Number(s.dataset.count), 0);
      let before = 0, lastYear = "", lastAt = -100;
      const marks = [];
      for (const section of months) {
        const year = section.dataset.month.slice(0, 4), at = before / total;
        if (year !== lastYear) {
          lastYear = year;
          marks.push({ year, at, section });
        }
        before += Number(section.dataset.count);
      }
      const here = document.createElement("i");
      const draw = () => {
        rail.textContent = "";
        lastAt = -100;
        for (const mark of marks) {
          const y = mark.at * rail.clientHeight;
          if (y - lastAt < 22) continue; // too close to the one above to read
          lastAt = y;
          const button = document.createElement("button");
          button.textContent = mark.year;
          button.style.top = y + "px";
          button.addEventListener("click", () => mark.section.scrollIntoView({ block: "start" }));
          rail.append(button);
        }
        rail.append(here);
        place();
      };
      const place = () => {
        const room = document.documentElement.scrollHeight - innerHeight;
        here.style.top = (room > 0 ? scrollY / room : 0) * rail.clientHeight + "px";
      };
      draw();
      addEventListener("resize", draw);
      addEventListener("scroll", place, { passive: true });
    }

    // Photos arriving from a phone, or changed on another device, show without reloading. The server
    // names the month of a new photo: a month on the page is fetched again; one the page does not have
    // yet means the page itself is. While a photo is open or some are chosen, it waits.
    if (window.EventSource) {
      let wait = null, reload = false;
      const refetch = (section) => fetch("/web/month/" + section.dataset.month + (main.dataset.query || location.search), { credentials: "same-origin" })
        .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
        .then((html) => { if (section.dataset.loaded) { const box = section.querySelector(".days"); box.innerHTML = html; dress(box); } })
        .catch(() => {});
      const apply = () => {
        wait = null;
        if ((viewer && viewer.open) || picked.size) { wait = setTimeout(apply, 2000); return; }
        if (reload) return location.reload();
        months.forEach((s) => { if (s.dataset.loaded) refetch(s); });
      };
      new EventSource("/web/events").addEventListener("message", (event) => {
        const { month } = JSON.parse(event.data);
        if (month && !months.some((s) => s.dataset.month === month) && !main.dataset.query) reload = true;
        clearTimeout(wait);
        wait = setTimeout(apply, 800); // a phone sends many at once: one fetch for the lot
      });
    }
  }

  // Uploading from this browser: the files go to the same door the phone app uses, one after another, so
  // that a large batch neither floods the server nor loses everything to one failure.
  const picker = document.querySelector(".upload input");
  if (picker) {
    const label = picker.closest(".upload"), text = label.querySelector("span");
    const send = async (files) => {
      files = Array.from(files).filter((f) => f.type.startsWith("image/") || f.type.startsWith("video/") || /\.(heic|heif|mov|mp4)$/i.test(f.name));
      if (!files.length || label.classList.contains("busy")) return;
      label.classList.add("busy");
      let done = 0, added = 0, failed = 0;
      const ids = []; // new and already there: both are put where this page shows them
      for (const file of files) {
        text.textContent = "올리는 중 " + (done + 1) + "/" + files.length;
        const form = new FormData(), when = new Date(file.lastModified || Date.now()).toISOString();
        form.append("fileCreatedAt", when);
        form.append("fileModifiedAt", when);
        form.append("assetData", file, file.name);
        try {
          const r = await fetch("/api/assets", { method: "POST", body: form, credentials: "same-origin" });
          if (r.status === 201) added++; else if (!r.ok) failed++;
          if (r.ok) { const body = await r.json().catch(() => ({})); if (body.id) ids.push(body.id); }
        } catch (e) { failed++; }
        done++;
      }
      // uploaded where it should show: into this album, shared on the shared tab, a favourite on that tab
      const show = new URLSearchParams(location.search).get("show"), album = document.body.dataset.album;
      let where = "";
      if (ids.length) {
        try {
          if (album) { await post("/web/album", { album, id: ids }); where = " · 이 앨범에 넣음"; }
          else if (show === "shared") { await post("/web/photos/share", { id: ids, on: 1 }); where = " · 공유함"; }
          else if (show === "favorites") { await post("/web/photos/favorite", { id: ids, on: 1 }); where = " · 즐겨찾기함"; }
        } catch (e) { where = " · 넣지 못함"; }
      }
      text.textContent = "올리기";
      say((failed ? added + "개 올림, " + failed + "개 실패" : added ? added + "개 올림" : "이미 있는 사진입니다") + where);
      label.classList.remove("busy");
      picker.value = "";
      if (added || where) setTimeout(() => location.reload(), 2500); // give the small pictures a moment to be made
    };
    picker.addEventListener("change", () => send(picker.files));
    document.querySelectorAll("[data-pick]").forEach((b) => b.addEventListener("click", () => picker.click())); // the empty page's drop area
    let depth = 0;
    addEventListener("dragenter", (e) => { if (e.dataTransfer && e.dataTransfer.types.includes("Files")) { depth++; document.body.classList.add("dropping"); } });
    addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; document.body.classList.remove("dropping"); } });
    addEventListener("dragover", (e) => { if (e.dataTransfer && e.dataTransfer.types.includes("Files")) e.preventDefault(); });
    // A dropped folder arrives as one item with no type: its files — in the folders inside it too — are
    // read through the entries, a hundred at a time.
    const walk = async (entry, out) => {
      try {
        if (entry.isFile) {
          out.push(await new Promise((ok, no) => entry.file(ok, no)));
        } else if (entry.isDirectory) {
          const reader = entry.createReader();
          for (let batch; (batch = await new Promise((ok, no) => reader.readEntries(ok, no))).length;) {
            for (const inner of batch) await walk(inner, out);
          }
        }
      } catch (err) { /* one unreadable file or folder does not stop the rest */ }
    };
    addEventListener("drop", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.files.length) return;
      e.preventDefault();
      depth = 0;
      document.body.classList.remove("dropping");
      // the entries must be taken now: the browser empties the list once this handler returns
      const entries = Array.from(e.dataTransfer.items || []).map((i) => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
      if (!entries.some((en) => en.isDirectory)) return send(e.dataTransfer.files);
      text.textContent = "폴더 읽는 중";
      (async () => {
        const files = [];
        for (const entry of entries) await walk(entry, files);
        text.textContent = "올리기";
        if (!files.length) return say("폴더에 사진이나 영상이 없습니다");
        send(files);
      })();
    });
  }

  // Buttons that cannot be taken back ask first.
  document.addEventListener("click", (event) => {
    const button = event.target.closest("button[data-confirm]");
    if (button && !confirm(button.dataset.confirm)) event.preventDefault();
  });
  // The account menu closes when something else is pressed.
  document.addEventListener("click", (event) => {
    const me = document.querySelector(".me[open]");
    if (me && !me.contains(event.target)) me.open = false;
  });

  const cells = () => Array.from(document.querySelectorAll("a[data-photo]"));
  const cellOf = (id) => document.querySelector('a[data-photo="' + id + '"]');

  // Choosing photos. The round mark on a photo chooses it; once something is chosen, pressing a photo
  // chooses too, Shift takes everything up to the last one pressed, and the bar on top acts on them all.
  const bar = document.querySelector(".selbar");
  let lastPicked = null;
  const refresh = () => {
    document.body.classList.toggle("picking", picked.size > 0);
    if (!bar) return;
    bar.querySelector(".count").textContent = picked.size + "장 고름";
    // someone else's photo among them: only what anyone may do stays. All shared already: offer to stop.
    const chosen = Array.from(picked, cellOf).filter(Boolean);
    document.body.classList.toggle("foreign", chosen.some((c) => c.classList.contains("theirs")));
    const share = bar.querySelector("[data-act=share] span");
    if (share) share.textContent = chosen.length && chosen.every((c) => c.classList.contains("shared")) ? "공유 풀기" : "공유하기";
  };
  const pick = (cell, on) => {
    if (on === undefined) on = !picked.has(cell.dataset.photo);
    if (on) picked.add(cell.dataset.photo); else picked.delete(cell.dataset.photo);
    cell.classList.toggle("picked", on);
  };
  const clear = () => { picked.clear(); lastPicked = null; cells().forEach((c) => c.classList.remove("picked")); refresh(); };
  const drop = (ids) => { ids.forEach((id) => { const c = cellOf(id); if (c) c.remove(); }); clear(); document.querySelectorAll(".flow").forEach(lay); };

  // One photo, over the page — the list stays where it was underneath. Stepping to the next photo walks
  // the cells of the list in the order they are shown, and moves the list along, so that the months
  // ahead are loaded by the time they are reached.
  const viewer = document.querySelector(".viewer");
  let current = null;
  let zoom = { s: 1, x: 0, y: 0 }; // how far in the photo is, and how far it has been moved, in the viewer
  const neighbour = (step) => { const all = cells(); return all[all.indexOf(current) + step] || null; };
  const show = (cell) => {
    if (!cell || !viewer) return;
    fetch("/web/photo/" + cell.dataset.photo, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
      .then((html) => {
        current = cell;
        cell.scrollIntoView({ block: "center" });
        viewer.innerHTML = html;
        zoom = { s: 1, x: 0, y: 0 };
        viewer.classList.remove("zoomed", "moving");
        viewer.querySelectorAll(".step").forEach((b) => { b.hidden = !neighbour(Number(b.dataset.step)); });
        if (!viewer.open) viewer.showModal();
        viewer.focus(); // not the first button: opened by a click, it would show a focus ring for nothing
      })
      .catch(() => {});
  };
  const step = (by) => show(neighbour(by));

  if (bar) {
    document.addEventListener("click", (event) => {
      const all = event.target.closest(".day h3 .pick");
      if (all) {
        const group = Array.from(all.closest(".day").querySelectorAll("a[data-photo]"));
        const on = !group.every((c) => picked.has(c.dataset.photo));
        group.forEach((c) => pick(c, on));
        refresh();
      }
    });
    bar.addEventListener("click", (event) => {
      const button = event.target.closest("[data-act]");
      if (!button) return;
      const ids = Array.from(picked), act = button.dataset.act;
      if (act === "clear") clear();
      if (act === "favorite") post("/web/photos/favorite", { id: ids, on: 1 }).then(() => {
        ids.forEach((id) => { const c = cellOf(id); if (c) c.classList.add("fav"); });
        clear();
        say(ids.length + "장을 즐겨찾기에 넣었습니다");
      }, failed);
      if (act === "share") {
        const on = button.querySelector("span").textContent === "공유하기";
        if (on && !confirm(ids.length + "장을 공유할까요? 이 서버를 쓰는 모든 사람이 보게 됩니다.")) return;
        post("/web/photos/share", on ? { id: ids, on: 1 } : { id: ids }).then(() => {
          if (!on && /show=shared/.test(location.search)) return location.reload();
          ids.forEach((id) => { const c = cellOf(id); if (c) c.classList.toggle("shared", on); });
          clear();
          say(on ? ids.length + "장을 공유했습니다" : ids.length + "장의 공유를 풀었습니다");
        }, failed);
      }
      if (act === "delete" && confirm(ids.length + "장을 휴지통으로 보낼까요? 다른 기기에서도 사라지고, 휴지통에서 되살릴 수 있습니다."))
        post("/web/photos/delete", { id: ids }).then(() => { drop(ids); say(ids.length + "장을 휴지통으로 보냈습니다"); }, failed);
      if (act === "restore") post("/web/photos/restore", { id: ids }).then(() => { drop(ids); say(ids.length + "장을 되살렸습니다"); }, failed);
      if (act === "unalbum") post("/web/album", { album: document.body.dataset.album, id: ids, remove: 1 }).then(() => { drop(ids); say(ids.length + "장을 앨범에서 뺐습니다"); }, failed);
      if (act === "download") { // one after another; the browser may ask once whether to allow several
        ids.forEach((id, i) => setTimeout(() => {
          const link = document.createElement("a");
          link.href = "/api/assets/" + id + "/original";
          link.download = "";
          document.body.append(link);
          link.click();
          link.remove();
        }, i * 400));
        clear();
      }
      if (act === "album") chooseAlbum(ids);
      if (act === "link") makeLink({ id: ids });
    });
  }

  const sheet = document.querySelector(".sheet");

  // A notice of the licenses page opens over the page, to be read and closed — the app's window has no
  // back button to come back from a bare text file.
  document.addEventListener("click", (event) => {
    const link = event.target.closest("a[data-notice]");
    if (!link || !sheet) return;
    event.preventDefault();
    fetch(link.href, { credentials: "same-origin" }).then((r) => (r.ok ? r.text() : Promise.reject(r.status))).then((text) => {
      sheet.innerHTML = '<header><h2></h2><button type="button" class="icon" data-act="cancel" aria-label="닫기"><svg class="i" aria-hidden="true"><use href="/static/icons.svg#close"/></svg></button></header><pre></pre>';
      sheet.querySelector("h2").textContent = link.textContent;
      sheet.querySelector("pre").textContent = text;
      sheet.querySelector("[data-act=cancel]").addEventListener("click", () => sheet.close());
      sheet.classList.add("reading");
      if (!sheet.open) sheet.showModal();
      sheet.querySelector("pre").scrollTop = 0;
    }, failed);
  });
  if (sheet) sheet.addEventListener("close", () => sheet.classList.remove("reading"));
  // A shared link: made for the photos chosen or for an album, then shown with its address to copy.
  const copyText = (text) => (navigator.clipboard && window.isSecureContext ? navigator.clipboard.writeText(text) : Promise.reject())
    .catch(() => {
      const area = document.createElement("textarea");
      area.value = text;
      area.style.position = "fixed";
      area.style.opacity = "0";
      (document.querySelector("dialog[open]") || document.body).append(area); // outside an open dialog the page is inert: nothing could be selected, and the clipboard would be emptied
      area.select();
      const ok = document.execCommand("copy");
      area.remove();
      return ok ? undefined : Promise.reject();
    });
  const makeLink = (fields) => {
    post("/web/links", fields).then((r) => r.json()).then(({ url }) => {
      sheet.innerHTML = '<header><h2>링크를 만들었습니다</h2><button type="button" class="icon" data-act="cancel" aria-label="닫기"><svg class="i" aria-hidden="true"><use href="/static/icons.svg#close"/></svg></button></header>'
        + '<div class="address"><code></code><button type="button" class="button" data-act="copy"><svg class="i" aria-hidden="true"><use href="/static/icons.svg#copy"/></svg><span>주소 복사</span></button></div>'
        + '<p class="dim">이 주소를 받은 사람은 로그인 없이 봅니다. 7일 뒤 닫히고, <a href="/links">공유 링크</a>에서 기한 · 비밀번호를 바꾸거나 닫을 수 있습니다.</p>';
      sheet.querySelector("code").textContent = url;
      sheet.querySelector("[data-act=cancel]").addEventListener("click", () => sheet.close());
      const button = sheet.querySelector("[data-act=copy]");
      button.addEventListener("click", () => copyText(url).then(() => { button.querySelector("span").textContent = "복사했습니다"; }, () => say("복사하지 못했습니다 — 주소를 길게 눌러 복사해 주세요")));
      if (!sheet.open) sheet.showModal();
      clear();
    }, (status) => say(status === 409 ? "tidoly 에 연결해야 링크로 공유할 수 있습니다" : status === 400 ? "자신의 사진만 링크로 공유할 수 있습니다" : "링크를 만들지 못했습니다"));
  };
  document.querySelectorAll("[data-link-album]").forEach((b) => b.addEventListener("click", () => makeLink({ album: b.dataset.linkAlbum })));

  const chooseAlbum = (ids) => {
    fetch("/web/albums", { credentials: "same-origin" }).then((r) => (r.ok ? r.text() : Promise.reject(r.status))).then((html) => {
      sheet.innerHTML = html;
      const put = (fields) => post("/web/album", Object.assign({ id: ids }, fields)).then(() => { sheet.close(); clear(); say(ids.length + "장을 앨범에 넣었습니다"); }, failed);
      sheet.querySelector("form").addEventListener("submit", (event) => { event.preventDefault(); put({ name: event.target.elements.name.value }); });
      sheet.querySelectorAll("[data-album]").forEach((b) => b.addEventListener("click", () => put({ album: b.dataset.album })));
      sheet.querySelector("[data-act=cancel]").addEventListener("click", () => sheet.close());
      sheet.showModal();
    }, failed);
  };

  // A choice in a form marked so is saved the moment it is made.
  document.addEventListener("change", (event) => {
    const form = event.target.closest("form[data-auto]");
    if (form) form.submit();
  });

  // Choosing a folder on the server's machine, one step at a time.
  const browse = document.querySelectorAll("[data-browse]");
  if (browse.length) {
    let purpose = ""; // "import": a folder of photos to read where it lies; otherwise a place to put photos
    let next = ""; // a first step to come back to, rather than the admin page
    const open = (at) => fetch("/admin/folders?for=" + purpose + "&at=" + encodeURIComponent(at) + "&next=" + encodeURIComponent(next), { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
      .then((html) => {
        sheet.innerHTML = html;
        sheet.classList.add("picker");
        if (!sheet.open) sheet.showModal();
      }, failed);
    browse.forEach((b) => b.addEventListener("click", () => { purpose = b.dataset.browse; next = b.dataset.next || ""; open(""); }));
    if (document.querySelector("[data-running]")) setTimeout(() => location.reload(), 2000); // a folder is being looked at: show how far
    sheet.addEventListener("click", (event) => {
      if (!sheet.classList.contains("picker")) return;
      const button = event.target.closest("button");
      if (button && button.dataset.at !== undefined) open(button.dataset.at);
      else if (button && button.dataset.act === "cancel") sheet.close();
    });
    sheet.addEventListener("input", (event) => { // show where the new folder would be
      const to = sheet.querySelector(".to[data-at]");
      if (!to || event.target.name !== "name") return;
      const name = event.target.value.trim(), at = to.dataset.at, sep = to.dataset.sep;
      to.querySelector("b").textContent = name ? at + (at.endsWith(sep) ? "" : sep) + name : at;
    });
    sheet.addEventListener("close", () => { sheet.classList.remove("picker"); sheet.innerHTML = ""; });
  }

  document.addEventListener("click", (event) => {
    const cell = event.target.closest("a[data-photo]");
    if (!cell) return;
    event.preventDefault();
    // the heart and the share button on a photo act at once; while photos are being chosen they rest
    const quick = event.target.closest("[data-quick]");
    if (quick && !picked.size) {
      const share = quick.dataset.quick === "share", mark = share ? "shared" : "fav", on = !cell.classList.contains(mark);
      const id = cell.dataset.photo;
      post(share ? "/web/photos/share" : "/web/photos/favorite", on ? { id, on: 1 } : { id }).then(() => {
        if (!on && /show=(shared|favorites)/.test(location.search)) return location.reload(); // it no longer belongs on this tab
        cell.classList.toggle(mark, on);
        if (share) say(on ? "공유했습니다. 이 서버를 쓰는 모든 사람이 봅니다" : "공유를 풀었습니다");
      }, failed);
      return;
    }
    if (bar && (picked.size || event.target.closest(".pick"))) {
      if (event.shiftKey && lastPicked && lastPicked.isConnected) {
        const all = cells(), a = all.indexOf(lastPicked), b = all.indexOf(cell);
        all.slice(Math.min(a, b), Math.max(a, b) + 1).forEach((c) => pick(c, true));
      } else pick(cell);
      lastPicked = cell;
      refresh();
    } else show(cell);
  });
  document.addEventListener("keydown", (event) => { if (event.key === "Escape" && picked.size && !(viewer && viewer.open)) clear(); });

  if (viewer) {
    // Looking closer: the wheel and a double click go in and out at the pointer, dragging moves the
    // photo. The first time in, the preview is swapped for the file itself where the browser can show it.
    const picture = () => viewer.querySelector(".stage img");
    const settle = () => {
      const img = picture(), stage = viewer.querySelector(".stage");
      if (!img) return;
      const roomX = Math.max(0, (img.offsetWidth * zoom.s - stage.clientWidth) / 2), roomY = Math.max(0, (img.offsetHeight * zoom.s - stage.clientHeight) / 2);
      zoom.x = Math.min(roomX, Math.max(-roomX, zoom.x));
      zoom.y = Math.min(roomY, Math.max(-roomY, zoom.y));
      img.style.transform = zoom.s === 1 ? "" : "translate(" + zoom.x + "px, " + zoom.y + "px) scale(" + zoom.s + ")";
      viewer.classList.toggle("zoomed", zoom.s > 1);
      if (zoom.s > 1 && img.dataset.full) {
        const full = new Image();
        full.onload = () => { if (img.isConnected) img.src = full.src; };
        full.src = img.dataset.full;
        delete img.dataset.full;
      }
    };
    const zoomTo = (scale, atX, atY) => {
      const stage = viewer.querySelector(".stage");
      if (!picture()) return;
      const box = stage.getBoundingClientRect(), s = Math.min(6, Math.max(1, scale));
      if (atX === undefined) { atX = box.left + box.width / 2; atY = box.top + box.height / 2; }
      const ux = (atX - box.left - box.width / 2 - zoom.x) / zoom.s, uy = (atY - box.top - box.height / 2 - zoom.y) / zoom.s;
      zoom = s === 1 ? { s: 1, x: 0, y: 0 } : { s, x: zoom.x - ux * (s - zoom.s), y: zoom.y - uy * (s - zoom.s) };
      settle();
    };
    viewer.addEventListener("wheel", (event) => {
      if (!event.target.closest(".stage") || !picture()) return;
      event.preventDefault();
      zoomTo(zoom.s * (event.deltaY < 0 ? 1.2 : 1 / 1.2), event.clientX, event.clientY);
    }, { passive: false });
    viewer.addEventListener("dblclick", (event) => {
      if (event.target === picture()) zoomTo(zoom.s > 1 ? 1 : 2.5, event.clientX, event.clientY);
    });
    let drag = null, dragged = false; // dragged: the press that just ended moved the photo, so it is not a click
    viewer.addEventListener("pointerdown", (event) => {
      dragged = false;
      if (event.target !== picture() || zoom.s === 1) return;
      drag = { x: event.clientX - zoom.x, y: event.clientY - zoom.y };
      event.target.setPointerCapture(event.pointerId);
    });
    viewer.addEventListener("pointermove", (event) => {
      if (!drag) return;
      dragged = true;
      zoom.x = event.clientX - drag.x;
      zoom.y = event.clientY - drag.y;
      settle();
    });
    viewer.addEventListener("pointerup", () => { drag = null; });
    viewer.addEventListener("pointercancel", () => { drag = null; });
    addEventListener("resize", settle);

    // A live photo: its few seconds of video play in the photo's place, then the photo is back.
    const stopLive = () => {
      const motion = viewer.querySelector(".motion");
      if (motion) { motion.pause(); motion.hidden = true; }
      viewer.classList.remove("moving");
    };
    const playLive = () => {
      const motion = viewer.querySelector(".motion");
      if (!motion) return;
      if (viewer.classList.contains("moving")) return stopLive();
      zoomTo(1);
      motion.hidden = false;
      motion.currentTime = 0;
      viewer.classList.add("moving");
      motion.onended = stopLive;
      motion.play().catch(() => { stopLive(); say("이 브라우저에서는 이 영상을 재생하지 못합니다"); });
    };
    viewer.addEventListener("mouseover", (event) => { // resting the pointer on LIVE plays it, as pressing does
      if (event.target.closest("[data-act=live]") && !viewer.classList.contains("moving")) playLive();
    });

    const act = (button) => {
      const id = button.dataset.id, cell = current;
      switch (button.dataset.act) {
        case "zoom": zoomTo(zoom.s > 1 ? 1 : 2.5); break;
        case "live": playLive(); break;
        case "close": viewer.close(); break;
        case "step": step(Number(button.dataset.step)); break;
        case "info": viewer.classList.toggle("with-info"); break;
        case "favorite": {
          const on = button.getAttribute("aria-pressed") !== "true";
          post("/web/photos/favorite", on ? { id, on: 1 } : { id }).then(() => {
            button.setAttribute("aria-pressed", on);
            if (cell) cell.classList.toggle("fav", on);
          }, failed);
          break;
        }
        case "share": {
          const on = button.getAttribute("aria-pressed") !== "true";
          if (on && !confirm("이 사진을 공유할까요? 이 서버를 쓰는 모든 사람이 보게 됩니다.")) return;
          post("/web/photos/share", on ? { id, on: 1 } : { id }).then(() => {
            button.setAttribute("aria-pressed", on);
            say(on ? "공유했습니다" : "공유를 풀었습니다");
            if (cell) cell.classList.toggle("shared", on);
          }, failed);
          break;
        }
        case "delete":
        case "restore": {
          const gone = button.dataset.act === "delete";
          if (gone && !confirm("이 사진을 휴지통으로 보낼까요? 다른 기기에서도 사라지고, 휴지통에서 되살릴 수 있습니다.")) return;
          const after = neighbour(1) || neighbour(-1);
          post(gone ? "/web/photos/delete" : "/web/photos/restore", { id }).then(() => {
            const flow = cell.parentElement;
            cell.remove();
            lay(flow);
            if (after) show(after); else viewer.close();
          }, failed);
        }
      }
    };
    viewer.addEventListener("click", (event) => {
      const button = event.target.closest("[data-act]");
      if (dragged) dragged = false;
      else if (button) act(button);
      else if (event.target === viewer || event.target.classList.contains("stage")) viewer.close();
    });
    viewer.addEventListener("keydown", (event) => {
      const press = (name) => { const b = viewer.querySelector('[data-act="' + name + '"]'); if (b) act(b); };
      if (event.key === "ArrowRight") step(1);
      if (event.key === "ArrowLeft") step(-1);
      if (event.key === "i") press("info");
      if (event.key === "l") press("live");
      if (event.key === "f") press("favorite");
      if (event.key === "Delete") press("delete");
    });
    let touchX = null;
    viewer.addEventListener("touchstart", (event) => { touchX = event.changedTouches[0].clientX; }, { passive: true });
    viewer.addEventListener("touchend", (event) => {
      const moved = event.changedTouches[0].clientX - touchX;
      if (touchX !== null && zoom.s === 1 && Math.abs(moved) > 60 && !event.target.closest("video, .info")) step(moved < 0 ? 1 : -1);
      touchX = null;
    }, { passive: true });
    viewer.addEventListener("close", () => { viewer.innerHTML = ""; current = null; }); // also stops a playing video
  }
})();
