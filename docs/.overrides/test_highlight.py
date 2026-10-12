"""Exercise the installed Chroma formatter with real KDL source."""

from html.parser import HTMLParser
import subprocess
import unittest
from unittest.mock import patch

from pymdownx.superfences import SuperFencesException

import coopr_highlight


class ParsedHighlight(HTMLParser):
    def __init__(self, rendered):
        super().__init__(convert_charrefs=True)
        self.tags = []
        self.text = []
        self.feed(rendered)

    def handle_starttag(self, tag, attrs):
        self.tags.append((tag, dict(attrs)))

    def handle_data(self, data):
        self.text.append(data)


class HighlightTests(unittest.TestCase):
    def render(self, source, **kwargs):
        return coopr_highlight.format(source, "kdl", "highlight", {}, None, **kwargs)

    def test_kdl_v2_tokens_and_source_roundtrip(self):
        source = '''copy "<file&name>" chmod=0o640 enabled=#true disabled=#false
run """
    echo '<hello & world>'
    """
label note=#"raw <text> & \\path"#
'''
        rendered = self.render(source)
        parsed = ParsedHighlight(rendered)
        self.assertEqual("".join(parsed.text), source)
        self.assertEqual(parsed.tags[:3], [
            ("div", {"class": "highlight"}),
            ("pre", {}),
            ("code", {"class": "language-kdl"}),
        ])
        self.assertIn('<span class="kc">#true</span>', rendered)
        self.assertIn('<span class="kc">#false</span>', rendered)
        self.assertIn('<span class="mo">0o640</span>', rendered)
        self.assertNotIn('<span class="err">', rendered)

    def test_attribute_values_are_escaped_and_preserved(self):
        classes = ['example"<&']
        rendered = self.render(
            'extend {}', classes=classes, id_value='block"<&',
            attrs={"data-caption": 'caption"<&'},
        )
        self.assertEqual(ParsedHighlight(rendered).tags[0], ("div", {
            "class": 'highlight example"<&', "id": 'block"<&',
            "data-caption": 'caption"<&',
        }))
        self.assertEqual(classes, ['example"<&'])
        self.assertIn("&quot;&lt;&amp;", rendered)

    def test_failed_subprocess_stops_superfences(self):
        failure = subprocess.CalledProcessError(1, ["chroma"], stderr="lexer failure")
        with patch("coopr_highlight.subprocess.run", side_effect=failure):
            with self.assertRaises(SuperFencesException) as raised:
                self.render("extend {}")
        self.assertIs(raised.exception.__cause__, failure)
        self.assertIn("lexer failure", str(raised.exception))

    def test_missing_chroma_stops_superfences(self):
        with patch("coopr_highlight.subprocess.run", side_effect=FileNotFoundError("chroma")):
            with self.assertRaises(SuperFencesException):
                self.render("extend {}")


if __name__ == "__main__":
    unittest.main()
