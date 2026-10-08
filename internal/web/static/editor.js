// Code editor for HCL: <textarea data-editor="hcl"> stays the form's field,
// with the same text laid under it in color, and Tab and Enter indent. Without
// JavaScript the textarea works as it is. Esc, then Tab, leaves the field.
(function () {
  var selector = 'textarea[data-editor="hcl"]';
  // A comment, a string, a constant, or a name with what follows it: "=" makes
  // it an attribute, a label or "{" a block.
  var token = /(#[^\n]*|\/\/[^\n]*|\/\*[\s\S]*?(?:\*\/|$))|("(?:[^"\\\n]|\\.)*"?)|(\b(?:true|false|null)\b|-?\b\d+(?:\.\d+)?\b)|([A-Za-z_][\w-]*)(?=\s*([={"]?))/g;

  function escape(s) {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  function highlight(text) {
    var out = "", last = 0, m;
    token.lastIndex = 0;
    while ((m = token.exec(text))) {
      var kind = m[1] ? "comment" : m[2] ? "string" : m[3] ? "constant" : m[5] === "=" ? "attr" : m[5] ? "keyword" : "";
      out += escape(text.slice(last, m.index));
      out += kind ? '<span class="tok-' + kind + '">' + escape(m[0]) + "</span>" : escape(m[0]);
      last = token.lastIndex;
    }
    // A <pre> drops a last empty line; the textarea keeps it.
    return out + escape(text.slice(last)) + (text.slice(-1) === "\n" ? " " : "");
  }

  // insert types text at the cursor, so the browser's undo keeps working.
  function insert(ta, text) {
    if (!document.execCommand("insertText", false, text)) {
      ta.setRangeText(text, ta.selectionStart, ta.selectionEnd, "end");
      ta.dispatchEvent(new Event("input"));
    }
  }

  function keys(e) {
    var ta = e.target, escaped = ta.dataset.escaped === "1";
    ta.dataset.escaped = e.key === "Escape" ? "1" : "";
    if (e.altKey || e.ctrlKey || e.metaKey || ta.selectionStart !== ta.selectionEnd) {
      return;
    }
    var at = ta.selectionStart, line = ta.value.slice(ta.value.lastIndexOf("\n", at - 1) + 1, at);
    if (e.key === "Tab" && !e.shiftKey && !escaped) {
      e.preventDefault();
      insert(ta, "  ");
    } else if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      insert(ta, "\n" + line.match(/^ */)[0] + (/[{[]\s*$/.test(line) ? "  " : ""));
    } else if ((e.key === "}" || e.key === "]") && /^ {2,}$/.test(line)) {
      e.preventDefault();
      ta.setSelectionRange(at - 2, at);
      insert(ta, e.key);
    }
  }

  function setup(ta) {
    if (ta.dataset.enhanced) {
      return;
    }
    ta.dataset.enhanced = "1";
    var wrap = document.createElement("div");
    var view = document.createElement("pre");
    var hint = document.createElement("p");
    wrap.className = "code-editor";
    view.className = "code-editor__view";
    view.setAttribute("aria-hidden", "true");
    hint.className = "field__caption";
    hint.textContent = "Tab indents. Esc, then Tab, leaves the field.";
    ta.parentNode.insertBefore(wrap, ta);
    wrap.appendChild(view);
    wrap.appendChild(ta);
    wrap.insertAdjacentElement("afterend", hint);
    ta.classList.add("code-editor__input");
    ta.setAttribute("wrap", "off");

    function sync() {
      view.scrollTop = ta.scrollTop;
      view.scrollLeft = ta.scrollLeft;
    }
    function paint() {
      view.innerHTML = highlight(ta.value);
      sync();
    }
    ta.addEventListener("input", paint);
    ta.addEventListener("scroll", sync);
    ta.addEventListener("keydown", keys);
    paint();
  }

  function enhance(root) {
    if (root.matches && root.matches(selector)) {
      setup(root);
    }
    if (root.querySelectorAll) {
      root.querySelectorAll(selector).forEach(setup);
    }
  }

  enhance(document);
  // htmx swaps in a new page without running this file again.
  document.addEventListener("htmx:load", function (e) {
    enhance(e.detail.elt);
  });
})();
