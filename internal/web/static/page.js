// What every page has, the setup and login pages too: an eye beside every password box, and copy
// buttons — the app's window has no address bar to copy the server's address from.
(function () {
  "use strict";
  const icon = (name) => '<svg class="i" aria-hidden="true"><use href="/static/icons.svg#' + name + '"/></svg>';

  // Pressed, the password shows; pressed again, it hides.
  document.querySelectorAll('input[type="password"]').forEach((box) => {
    const wrap = document.createElement("span");
    wrap.className = "pw";
    box.replaceWith(wrap);
    wrap.append(box);
    const eye = document.createElement("button");
    eye.type = "button";
    eye.className = "eye";
    eye.setAttribute("aria-label", "비밀번호 보기");
    eye.innerHTML = icon("eye");
    eye.addEventListener("click", () => {
      const shown = box.type === "text";
      box.type = shown ? "password" : "text";
      eye.setAttribute("aria-label", shown ? "비밀번호 보기" : "비밀번호 숨기기");
      eye.innerHTML = icon(shown ? "eye" : "eye-off");
      box.focus();
    });
    wrap.append(eye);
  });

  // data-refresh="5" on something on its way: the page looks again in that many seconds, unless one is
  // typing in it.
  const busy = document.querySelector("[data-refresh]");
  if (busy) {
    setTimeout(() => {
      const active = document.activeElement;
      if (!active || !/^(INPUT|TEXTAREA|SELECT)$/.test(active.tagName)) location.reload();
    }, Number(busy.dataset.refresh) * 1000);
  }

  // An update on its way: the page asks the server every second. It answers with the old version while the
  // new one is downloaded, not at all while it is put in place, then with the new version.
  const update = document.querySelector("[data-update]");
  if (update) {
    const steps = update.querySelectorAll(".steps li");
    const say = update.querySelector("[data-say]");
    const at = (n) => steps.forEach((li, i) => { li.className = i < n ? "done" : i === n ? "on" : ""; });
    const end = (text) => { clearInterval(ask); say.textContent = text; update.querySelector("[data-done]").hidden = false; };
    let down = false;
    const ask = setInterval(() => {
      fetch("/admin/update/state", { credentials: "same-origin", cache: "no-store" })
        .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
        .then((s) => {
          if (s.version !== update.dataset.from) {
            at(steps.length);
            end(`${s.version} 으로 업데이트했습니다.`);
          } else if (s.state === "failed" || down) {
            end("업데이트하지 못했습니다. 잠시 뒤에 관리 화면에서 다시 해 주세요.");
          }
        }, () => {
          if (!down) say.textContent = "새 버전으로 바꿔 넣고 다시 여는 중입니다…";
          down = true;
          at(1);
        });
    }, 1000);
  }

  // Connecting to tidoly from the app's window: tidoly's sign-in happens in the browser. The window gets a
  // link good once, opens it there, and looks again once the connection is made.
  document.addEventListener("click", (event) => {
    const link = event.target.closest("a[data-connect]");
    if (!link || typeof window.stowOpen !== "function") return; // in a browser the link goes there itself
    event.preventDefault();
    fetch("/admin/tidoly/connect-link", { method: "POST", credentials: "same-origin" })
      .then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
      .then(({ url }) => {
        window.stowOpen(location.origin + url);
        link.textContent = "브라우저에서 연결 중…";
        const wait = setInterval(() => {
          fetch("/admin/tidoly/state", { credentials: "same-origin" }).then((r) => r.json()).then((s) => {
            if (s.connected) { clearInterval(wait); location.reload(); }
          }, () => {});
        }, 3000);
        setTimeout(() => clearInterval(wait), 10 * 60 * 1000);
      }, () => { location.href = link.href; }); // no link to be had: connect in this window, as before
  });

  // In the app's window (it gives the pages stowOpen), a link that leaves the server or asks for a new window
  // opens in the browser: the window has no address bar nor back button. In a browser, nothing changes.
  document.addEventListener("click", (event) => {
    const link = event.target.closest("a[href]");
    if (!link || typeof window.stowOpen !== "function" || event.defaultPrevented) return;
    const to = new URL(link.href, location.href);
    if (!/^https?:$/.test(to.protocol) || (to.origin === location.origin && link.target !== "_blank")) return;
    event.preventDefault();
    window.stowOpen(to.href);
  });

  // <button data-copy="text">: copies the text and says so on the button for a moment.
  const oldWay = (text) => { // for a page over plain http, or a window that refuses the clipboard
    const area = document.createElement("textarea");
    area.value = text;
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.append(area);
    area.select();
    const ok = document.execCommand("copy");
    area.remove();
    return ok ? Promise.resolve() : Promise.reject();
  };
  const copy = (text) => navigator.clipboard && window.isSecureContext
    ? navigator.clipboard.writeText(text).catch(() => oldWay(text))
    : oldWay(text);
  document.querySelectorAll("[data-copy]").forEach((b) => b.addEventListener("click", () => {
    const label = b.querySelector("span") || b, before = label.textContent;
    copy(b.dataset.copy).then(() => { label.textContent = "복사했습니다"; }, () => { label.textContent = "복사하지 못했습니다"; });
    setTimeout(() => { label.textContent = before; }, 1800);
  }));
})();
