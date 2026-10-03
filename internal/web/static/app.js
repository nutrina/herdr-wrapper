// Small enhancements for the task form and detail page. Everything here is
// optional: the forms still submit correctly with JavaScript disabled.

// Label field: turns the comma-separated hidden value into removable chips.
document.querySelectorAll('[data-tokens]').forEach(function (box) {
  var hidden = box.querySelector('input[type="hidden"]');
  var entry = box.querySelector('input[type="text"]');
  var labels = hidden.value ? hidden.value.split(',') : [];

  function render() {
    box.querySelectorAll('.token').forEach(function (chip) { chip.remove(); });
    labels.forEach(function (label, index) {
      var chip = document.createElement('span');
      chip.className = 'token';
      chip.appendChild(document.createTextNode(label));

      var remove = document.createElement('button');
      remove.type = 'button';
      remove.textContent = '×';
      remove.setAttribute('aria-label', 'Remove label ' + label);
      remove.addEventListener('click', function () {
        labels.splice(index, 1);
        render();
        entry.focus();
      });

      chip.appendChild(remove);
      box.insertBefore(chip, entry);
    });
    hidden.value = labels.join(',');
  }

  function add() {
    entry.value.split(',').forEach(function (part) {
      var label = part.trim();
      if (label && labels.indexOf(label) < 0) labels.push(label);
    });
    entry.value = '';
    render();
  }

  entry.addEventListener('keydown', function (event) {
    if (event.key === 'Enter' || event.key === ',') {
      event.preventDefault();
      add();
    } else if (event.key === 'Backspace' && !entry.value && labels.length) {
      labels.pop();
      render();
    }
  });
  entry.addEventListener('blur', add);

  render();
});

// Description editor: the toolbar's Preview button swaps the textarea for the
// rendered markdown, fetched from the server each time it is switched on.
document.querySelectorAll('[data-editor]').forEach(function (editor) {
  var toggle = editor.querySelector('[data-preview-toggle]');
  var area = editor.querySelector('textarea');
  var preview = editor.querySelector('[data-pane="preview"]');
  if (!toggle) return;

  toggle.addEventListener('click', function () {
    var show = toggle.getAttribute('aria-pressed') !== 'true';
    toggle.setAttribute('aria-pressed', show ? 'true' : 'false');
    area.hidden = show;
    preview.hidden = !show;
    editor.querySelectorAll('[data-md]').forEach(function (button) { button.disabled = show; });

    if (show) {
      htmx.ajax('POST', '/preview', { target: preview, swap: 'innerHTML', values: { description: area.value } });
    } else {
      area.focus();
    }
  });
});

// Markdown toolbar: each button wraps the selection or prefixes the selected lines.
document.querySelectorAll('[data-editor]').forEach(function (editor) {
  var area = editor.querySelector('textarea');
  if (!area) return;

  // Replaces [start, end) with text and selects [selStart, selEnd).
  // insertText keeps the browser's undo history; setRangeText is the fallback.
  function replace(start, end, text, selStart, selEnd) {
    area.focus();
    area.setSelectionRange(start, end);
    if (!document.execCommand('insertText', false, text)) {
      area.setRangeText(text, start, end, 'preserve');
    }
    area.setSelectionRange(selStart, selEnd);
  }

  function wrap(before, after, placeholder) {
    var start = area.selectionStart, end = area.selectionEnd;
    var inner = area.value.slice(start, end) || placeholder;
    replace(start, end, before + inner + after, start + before.length, start + before.length + inner.length);
  }

  // Adds a prefix to every selected line, or removes it if all of them already have it.
  function prefixLines(pattern, prefixFor) {
    var value = area.value;
    var start = value.lastIndexOf('\n', area.selectionStart - 1) + 1;
    var end = value.indexOf('\n', area.selectionEnd);
    if (end < 0) end = value.length;

    var lines = value.slice(start, end).split('\n');
    var allPrefixed = lines.every(function (line) { return pattern.test(line); });
    var text = lines.map(function (line, index) {
      return allPrefixed ? line.replace(pattern, '') : prefixFor(index) + line;
    }).join('\n');

    replace(start, end, text, start, start + text.length);
  }

  var actions = {
    bold: function () { wrap('**', '**', 'bold text'); },
    italic: function () { wrap('_', '_', 'italic text'); },
    code: function () { wrap('`', '`', 'code'); },
    heading: function () { prefixLines(/^#{1,6} /, function () { return '## '; }); },
    bullets: function () { prefixLines(/^- (?!\[[ x]\] )/, function () { return '- '; }); },
    numbers: function () { prefixLines(/^\d+\. /, function (index) { return (index + 1) + '. '; }); },
    tasks: function () { prefixLines(/^- \[[ x]\] /, function () { return '- [ ] '; }); },
    quote: function () { prefixLines(/^> /, function () { return '> '; }); },
    link: function () {
      var start = area.selectionStart, end = area.selectionEnd;
      var label = area.value.slice(start, end) || 'link text';
      var urlStart = start + label.length + 3;
      replace(start, end, '[' + label + '](url)', urlStart, urlStart + 3);
    },
    codeblock: function () {
      var start = area.selectionStart, end = area.selectionEnd;
      var inner = area.value.slice(start, end) || 'code';
      var lead = start > 0 && area.value[start - 1] !== '\n' ? '\n' : '';
      var open = lead + '```\n';
      replace(start, end, open + inner + '\n```\n', start + open.length, start + open.length + inner.length);
    }
  };

  editor.querySelectorAll('[data-md]').forEach(function (button) {
    button.addEventListener('click', function () {
      actions[button.getAttribute('data-md')]();
    });
  });

  var shortcuts = { b: 'bold', i: 'italic', k: 'link' };
  area.addEventListener('keydown', function (event) {
    var name = shortcuts[event.key.toLowerCase()];
    if (name && (event.metaKey || event.ctrlKey) && !event.altKey && !event.shiftKey) {
      event.preventDefault();
      actions[name]();
    }
  });
});

// Drop zone: lists the files chosen for upload and highlights while dragging.
document.querySelectorAll('[data-dropzone]').forEach(function (zone) {
  var input = zone.querySelector('input[type="file"]');
  var chosen = zone.querySelector('[data-chosen]');

  input.addEventListener('change', function () {
    chosen.textContent = '';
    Array.prototype.forEach.call(input.files, function (file) {
      var item = document.createElement('li');
      item.textContent = file.name;
      chosen.appendChild(item);
    });
  });

  ['dragenter', 'dragover'].forEach(function (name) {
    zone.addEventListener(name, function () { zone.classList.add('is-over'); });
  });
  ['dragleave', 'drop'].forEach(function (name) {
    zone.addEventListener(name, function () { zone.classList.remove('is-over'); });
  });
});

// Detail page: upload as soon as files are picked.
document.querySelectorAll('[data-autosubmit]').forEach(function (input) {
  input.addEventListener('change', function () {
    if (input.files.length) input.form.submit();
  });
});
