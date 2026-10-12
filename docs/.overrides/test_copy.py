"""Build-time clipboard controls preserve code and native opt-out behavior."""

from html.parser import HTMLParser
import unittest

import markdown

from coopr_copy import _CopyControls


class Elements(HTMLParser):
    def __init__(self, source):
        super().__init__()
        self.elements = []
        self.feed(source)

    def handle_starttag(self, tag, attrs):
        self.elements.append((tag, dict(attrs)))

    def attrs(self, tag):
        return [attrs for name, attrs in self.elements if name == tag]


class CopyControlsTests(unittest.TestCase):
    def render(self, source):
        return _CopyControls().run(source)

    def test_controls_target_code_and_preserve_highlighted_content(self):
        body = '<span class="k">echo</span> &quot;λ &amp; 世界&quot;\n'
        source = f'<div class="highlight"><pre data-test="yes"><code class="language-sh">{body}</code></pre></div>'
        result = self.render(source)
        elements = Elements(result)
        code = elements.attrs("code")[0]
        button = elements.attrs("button")[0]
        self.assertEqual(code["class"], "language-sh")
        self.assertEqual(elements.attrs("pre"), [{"data-test": "yes"}])
        self.assertEqual(button["data-clipboard-target"], f'[id="{code["id"]}"]')
        self.assertEqual(button["data-md-type"], "copy")
        self.assertEqual(button["type"], "button")
        self.assertEqual(button["title"], button["aria-label"])
        self.assertIn(f'>{body}</code>', result)
        self.assertIn('</nav><code', result)

    def test_inline_no_copy_mermaid_and_pyodide_are_untouched(self):
        for source in (
            '<p><code>inline</code></p>',
            '<div class="highlight no-copy"><pre><code>output</code></pre></div>',
            '<pre><code class="no-copy">output</code></pre>',
            '<pre class="mermaid"><code>graph TD</code></pre>',
            '<div class="pyodide"><pre><code>print(1)</code></pre></div>',
        ):
            with self.subTest(source=source):
                self.assertEqual(self.render(source), source)

    def test_explicit_copy_overrides_no_copy_and_consumes_marker(self):
        source = '<div class="highlight no-copy copy" id="sample"><pre class="copy"><code>x</code></pre></div>'
        result = self.render(source)
        elements = Elements(result)
        self.assertEqual(len(elements.attrs("button")), 1)
        for _, attrs in elements.elements:
            self.assertNotIn("copy", attrs.get("class", "").split())
        self.assertIn("no-copy", elements.attrs("div")[0]["class"])
        self.assertEqual(self.render(result), result)

    def test_ids_include_later_authored_collisions_and_preserve_existing(self):
        source = '<pre><code>one</code></pre><p id="coopr-code-1">reserved</p><pre><code id="coopr-code-2">two</code></pre><pre><code>three</code></pre>'
        elements = Elements(self.render(source))
        ids = [attrs["id"] for attrs in elements.attrs("code")]
        self.assertEqual(ids, ["coopr-code-3", "coopr-code-2", "coopr-code-4"])
        self.assertEqual([attrs["data-clipboard-target"] for attrs in elements.attrs("button")], [f'[id="{identifier}"]' for identifier in ids])

    def test_code_copy_overrides_ancestor_opt_out_and_preserves_attributes(self):
        for identifier in ("", ' id="authored"'):
            source = (
                '<div class="no-copy"><pre>'
                f'<code class="language-sh copy" data-test="yes"{identifier}>value</code>'
                '</pre></div>'
            )
            with self.subTest(identifier=identifier):
                elements = Elements(self.render(source))
                code = elements.attrs("code")[0]
                self.assertEqual(code["class"], "language-sh")
                self.assertEqual(code["data-test"], "yes")
                self.assertEqual(code["id"], "authored" if identifier else "coopr-code-1")
                self.assertEqual(len(elements.attrs("button")), 1)

    def test_authored_id_uses_escaped_attribute_selector(self):
        source = '<pre><code id="1:a &quot;quote&quot; \\ slash">value</code></pre>'
        elements = Elements(self.render(source))
        self.assertEqual(elements.attrs("code")[0]["id"], '1:a "quote" \\ slash')
        self.assertEqual(elements.attrs("button")[0]["data-clipboard-target"], '[id="1:a \\"quote\\" \\\\ slash"]')

    def test_empty_authored_id_is_replaced_without_duplicate_attribute(self):
        result = self.render('<pre><code id="">value</code></pre>')
        self.assertIn('<code id="coopr-code-1">', result)

    def test_real_superfences_titled_and_opt_out_blocks(self):
        source = '```sh title="Example"\necho "λ & world"\n```\n\n```{ .text .no-copy }\noutput\n```\n\n```{ .sh .copy .no-copy }\necho explicit\n```\n\nInline `value`.\n'
        result = markdown.markdown(source, extensions=["pymdownx.superfences", "coopr_copy"])
        elements = Elements(result)
        self.assertEqual(len(elements.attrs("button")), 2)
        self.assertIn('class="filename">Example', result)
        self.assertIn('<code>value</code>', result)
        self.assertIn('output\n</code>', result)
        self.assertIn('λ', result)
        self.assertNotIn("copy", elements.attrs("div")[-1].get("class", "").split())


if __name__ == "__main__":
    unittest.main()
