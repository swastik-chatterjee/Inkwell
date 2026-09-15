/*
 * Inkwell — progressive enhancement only.
 *
 * Everything here is optional. With JavaScript disabled the application still
 * works completely: ordinary links, ordinary forms, full page loads. There is
 * no framework, no bundler, no build step — just this one file.
 */
(function () {
    'use strict';

    // ------------------------------------------------------------------
    // 1. Minimal, approximate Markdown preview
    // ------------------------------------------------------------------
    // A tiny client-side renderer so authors can see roughly what their
    // Markdown will look like while typing. The server re-renders and
    // sanitizes with Goldmark + bluemonday before storing anything; this
    // preview is never trusted and never persisted. All input is HTML-escaped
    // before the Markdown transformations run, so nothing is ever injected
    // as markup here either.

    function escapeHtml(text) {
        return String(text).replace(/[&<>"']/g, function (ch) {
            return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch];
        });
    }

    function renderInline(text) {
        var out = text;
        out = out.replace(/`([^`]+)`/g, function (m, code) { return '<code>' + code + '</code>'; });
        out = out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
        out = out.replace(/\*([^*]+)\*/g, '<em>$1</em>');
        out = out.replace(/~~([^~]+)~~/g, '<del>$1</del>');
        out = out.replace(/\[([^\]]*)\]\(([^)\s]*)\)/g, function (m, label, href) {
            // Only http(s) and same-origin-relative URLs in the preview.
            if (/^(https?:\/\/|\/)/i.test(href)) {
                return '<a href="' + href + '" rel="nofollow noopener">' + (label || href) + '</a>';
            }
            return label || href;
        });
        return out;
    }

    function renderMarkdown(source) {
        var lines = escapeHtml(source).split(/\r?\n/);
        var out = [];
        var para = [];
        var code = null; // non-null array while inside a fenced block
        var listTag = null;
        var listItems = [];
        var quote = [];

        function flushPara() {
            if (para.length) { out.push('<p>' + renderInline(para.join(' ')) + '</p>'); para = []; }
        }
        function flushList() {
            if (listTag) {
                out.push('<' + listTag + '>' + listItems.map(function (item) {
                    return '<li>' + renderInline(item) + '</li>';
                }).join('') + '</' + listTag + '>');
                listTag = null;
                listItems = [];
            }
        }
        function flushQuote() {
            if (quote.length) { out.push('<blockquote><p>' + renderInline(quote.join(' ')) + '</p></blockquote>'); quote = []; }
        }
        function flushAll() { flushPara(); flushList(); flushQuote(); }

        for (var i = 0; i < lines.length; i++) {
            var line = lines[i];

            if (/^\s*```/.test(line)) {
                if (code) {
                    out.push('<pre><code>' + code.join('\n') + '</code></pre>');
                    code = null;
                } else {
                    flushAll();
                    code = [];
                }
                continue;
            }
            if (code) { code.push(line); continue; }

            if (/^\s*$/.test(line)) { flushAll(); continue; }

            var heading = line.match(/^(#{1,6})\s+(.*)$/);
            if (heading) {
                flushAll();
                var level = heading[1].length;
                out.push('<h' + level + '>' + renderInline(heading[2]) + '</h' + level + '>');
                continue;
            }

            if (/^\s*(-{3,}|\*{3,})\s*$/.test(line)) { flushAll(); out.push('<hr>'); continue; }

            // ">" was escaped to "&gt;" by escapeHtml, so match that form.
            var quoted = line.match(/^&gt;\s?(.*)$/);
            if (quoted) { flushPara(); flushList(); quote.push(quoted[1]); continue; }

            var unordered = line.match(/^\s*[-*+]\s+(.*)$/);
            if (unordered) {
                flushPara(); flushQuote();
                if (listTag !== 'ul') { flushList(); listTag = 'ul'; }
                listItems.push(unordered[1]);
                continue;
            }

            var ordered = line.match(/^\s*\d+[.)]\s+(.*)$/);
            if (ordered) {
                flushPara(); flushQuote();
                if (listTag !== 'ol') { flushList(); listTag = 'ol'; }
                listItems.push(ordered[1]);
                continue;
            }

            flushList(); flushQuote();
            para.push(line.trim());
        }

        if (code) { out.push('<pre><code>' + code.join('\n') + '</code></pre>'); }
        flushAll();
        return out.join('\n');
    }

    function initPreviews() {
        var sources = document.querySelectorAll('textarea[data-markdown-source]');
        Array.prototype.forEach.call(sources, function (source) {
            var id = source.getAttribute('data-markdown-source');
            var box = document.querySelector('[data-markdown-preview="' + id + '"]');
            if (!box) { return; }
            var target = box.querySelector('.preview-body');
            if (!target) { return; }

            function update() {
                var value = source.value;
                if (!value.trim()) {
                    box.hidden = true;
                    target.textContent = '';
                    return;
                }
                box.hidden = false;
                target.innerHTML = renderMarkdown(value);
            }
            source.addEventListener('input', update);
            update();
        });
    }

    // Gentle auto-grow for the source textareas.
    function initAutoGrow() {
        var areas = document.querySelectorAll('textarea[data-markdown-source]');
        Array.prototype.forEach.call(areas, function (area) {
            function grow() {
                area.style.height = 'auto';
                area.style.height = Math.min(area.scrollHeight, 640) + 'px';
            }
            area.addEventListener('input', grow);
            grow();
        });
    }

    // ------------------------------------------------------------------
    // 2. Confirmations (posts, comments, account deletion)
    // ------------------------------------------------------------------
    // The server is the authority: the account deletion POST is rejected
    // server-side unless confirm=DELETE. This is purely friendlier UX.

    var confirmMessages = {
        'delete-post': 'Delete this post? This cannot be undone.',
        'delete-comment': 'Delete this comment? This cannot be undone.'
    };

    function showInlineError(form, message) {
        var el = form.querySelector('.form-error');
        if (!el) {
            el = document.createElement('p');
            el.className = 'form-error';
            el.setAttribute('role', 'alert');
            form.insertBefore(el, form.firstChild);
        }
        el.textContent = message;
    }

    function initConfirmations() {
        document.addEventListener('submit', function (event) {
            var form = event.target;
            if (!form || !form.getAttribute) { return; }

            var kind = form.getAttribute('data-confirm');
            if (!kind) { return; }

            if (kind === 'delete-account') {
                var input = form.querySelector('[data-confirm-input]');
                if (input && input.value.trim().toUpperCase() !== 'DELETE') {
                    event.preventDefault();
                    showInlineError(form, 'Please type DELETE to confirm.');
                    input.focus();
                    return;
                }
                if (!window.confirm('Permanently delete your account, including all posts, comments, and likes? This cannot be undone.')) {
                    event.preventDefault();
                }
                return;
            }

            var message = confirmMessages[kind] || 'Are you sure?';
            if (!window.confirm(message)) {
                event.preventDefault();
            }
        });
    }

    // ------------------------------------------------------------------
    // 3. Progressive like/unlike
    // ------------------------------------------------------------------
    // The like forms work perfectly as normal POST/redirect/GET. When fetch
    // is available we submit in the background, follow the server's redirect,
    // and swap in the freshly rendered like form from the response document.
    // If anything fails, we fall back to a full form submission.

    var liveRegion = null;
    function announce(message) {
        if (!liveRegion) {
            liveRegion = document.createElement('p');
            liveRegion.setAttribute('role', 'status');
            liveRegion.setAttribute('aria-live', 'polite');
            liveRegion.className = 'sr-only';
            document.body.appendChild(liveRegion);
        }
        liveRegion.textContent = '';
        window.setTimeout(function () { liveRegion.textContent = message; }, 50);
    }

    function exchangeLike(form) {
        if (form.dataset.busy === '1') { return; }
        form.dataset.busy = '1';

        var data = new FormData(form);
        fetch(form.action, {
            method: 'POST',
            body: data,
            credentials: 'same-origin',
            headers: { 'Accept': 'text/html' }
        }).then(function (response) {
            if (!response.ok) { throw new Error('HTTP ' + response.status); }
            return response.text();
        }).then(function (html) {
            var doc = new DOMParser().parseFromString(html, 'text/html');
            var current = form.querySelector('.like-button');
            var postId = current ? current.getAttribute('data-post-id') : null;
            var fresh = postId ? doc.querySelector('.like-button[data-post-id="' + postId + '"]') : null;
            if (fresh && fresh.closest && form.parentNode) {
                var freshForm = fresh.closest('.like-form');
                form.parentNode.replaceChild(freshForm, form);
                announce(fresh.classList.contains('is-liked') ? 'Added to your likes.' : 'Removed from your likes.');
            } else {
                window.location.assign(form.action);
            }
        }).catch(function () {
            // Network or parsing trouble: fall back to a full submission.
            delete form.dataset.busy;
            form.submit();
        });
    }

    function initLikeForms() {
        document.addEventListener('submit', function (event) {
            var form = event.target;
            if (!form || !form.classList || !form.classList.contains('like-form')) { return; }
            if (!window.fetch || !window.DOMParser || !window.FormData) { return; }
            event.preventDefault();
            exchangeLike(form);
        });
    }

    // ------------------------------------------------------------------
    // Boot
    // ------------------------------------------------------------------
    function start() {
        initPreviews();
        initAutoGrow();
        initConfirmations();
        initLikeForms();
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', start);
    } else {
        start();
    }
})();
