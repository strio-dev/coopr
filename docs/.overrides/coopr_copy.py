"""Render native clipboard controls before the page's JavaScript starts."""

from html import escape
from html.parser import HTMLParser

from markdown.extensions import Extension
from markdown.postprocessors import Postprocessor


class _Blocks(HTMLParser):
    def __init__(self, source):
        super().__init__(convert_charrefs=False)
        self.lines = [0]
        self.lines.extend(index + 1 for index, char in enumerate(source) if char == "\n")
        self.stack = []
        self.ids = set()
        self.blocks = []

    def handle_starttag(self, tag, attrs):
        attributes = dict(attrs)
        if attributes.get("id"):
            self.ids.add(attributes["id"])
        line, column = self.getpos()
        node = {
            "tag": tag,
            "attrs": attrs,
            "classes": set((attributes.get("class") or "").split()),
            "start": self.lines[line - 1] + column,
            "raw": self.get_starttag_text(),
            "children": [],
        }
        if self.stack:
            self.stack[-1]["children"].append(tag)
        if tag == "code" and self.stack and self.stack[-1]["tag"] == "pre":
            self.blocks.append((node, list(self.stack)))
        if tag not in {
            "area", "base", "br", "col", "embed", "hr", "img", "input", "link",
            "meta", "param", "source", "track", "wbr",
        }:
            self.stack.append(node)

    def handle_startendtag(self, tag, attrs):
        self.handle_starttag(tag, attrs)
        if self.stack and self.stack[-1]["tag"] == tag:
            self.stack.pop()

    def handle_endtag(self, tag):
        for index in range(len(self.stack) - 1, -1, -1):
            if self.stack[index]["tag"] == tag:
                del self.stack[index:]
                break


class _CopyControls(Postprocessor):
    def run(self, text):
        parsed = _Blocks(text)
        parsed.feed(text)
        edits = {}
        changed = {}
        next_id = 1
        for code, ancestors in parsed.blocks:
            pre = ancestors[-1]
            nodes = [*ancestors, code]
            classes = set().union(*(node["classes"] for node in nodes))
            explicit = "copy" in classes
            if (
                "pyodide" in classes or "mermaid" in pre["classes"]
                or ("no-copy" in classes and not explicit)
            ):
                continue
            if "nav" in pre["children"]:
                continue
            for node in nodes:
                if "copy" in node["classes"]:
                    attrs = []
                    for key, value in node["attrs"]:
                        if key == "class":
                            value = " ".join(
                                item for item in (value or "").split() if item != "copy"
                            )
                        attrs.append((key, value))
                    changed[node["start"]] = (node, attrs)
            identifier = dict(code["attrs"]).get("id")
            if not identifier:
                while f"coopr-code-{next_id}" in parsed.ids:
                    next_id += 1
                identifier = f"coopr-code-{next_id}"
                parsed.ids.add(identifier)
                next_id += 1
                attrs = changed.get(code["start"], (code, code["attrs"]))[1]
                attrs = [(key, value) for key, value in attrs if key != "id"]
                changed[code["start"]] = (code, [*attrs, ("id", identifier)])
            # An attribute selector supports authored IDs that are not CSS identifiers.
            selector_id = ""
            for char in identifier:
                if char in '\\"':
                    selector_id += "\\" + char
                elif ord(char) < 32:
                    selector_id += f"\\{ord(char):x} "
                else:
                    selector_id += char
            target = escape(f'[id="{selector_id}"]', quote=True)
            control = (
                '<nav class="md-code__nav"><button class="md-code__button" type="button" '
                'title="Copy to clipboard" aria-label="Copy to clipboard" '
                f'data-md-type="copy" data-clipboard-target="{target}"></button></nav>'
            )
            edits[code["start"]] = (0, control)
        for position, (node, attrs) in changed.items():
            attributes = ""
            for key, value in attrs:
                attributes += f' {escape(key, quote=True)}'
                if value is not None:
                    attributes += f'="{escape(value, quote=True)}"'
            replacement = f'<{node["tag"]}{attributes}>'
            prefix = edits.get(position, (0, ""))[1]
            edits[position] = (len(node["raw"]), prefix + replacement)
        for position, (length, replacement) in sorted(edits.items(), reverse=True):
            text = text[:position] + replacement + text[position + length:]
        return text


class CopyControlsExtension(Extension):
    def extendMarkdown(self, md):
        md.postprocessors.register(_CopyControls(md), "coopr_copy", 0)


def makeExtension(**kwargs):
    return CopyControlsExtension(**kwargs)
