"""Render KDL fences with Chroma and the site's existing highlight styles."""

from html import escape
import subprocess

from pymdownx.superfences import SuperFencesException


def format(source, language, class_name, options, md, **kwargs):
    """SuperFences formatter; fail the build when Chroma cannot highlight."""
    try:
        highlighted = subprocess.run(
            ["chroma", "--lexer", "kdl", "--html", "--html-only",
             "--html-prevent-surrounding-pre"],
            input=source, text=True, capture_output=True, check=True,
        ).stdout
    except (OSError, subprocess.CalledProcessError) as error:
        detail = (error.stderr or str(error)) if isinstance(error, subprocess.CalledProcessError) else str(error)
        raise SuperFencesException(f"Chroma KDL highlighting failed: {detail}") from error

    classes = [class_name or "highlight", *kwargs.get("classes", [])]
    attributes = {"class": " ".join(classes)}
    if kwargs.get("id_value"):
        attributes["id"] = kwargs["id_value"]
    attributes.update(kwargs.get("attrs", {}))
    rendered_attributes = "".join(
        f' {escape(key, quote=True)}="{escape(str(value), quote=True)}"'
        for key, value in attributes.items()
    )
    return (
        f'<div{rendered_attributes}><pre><code class="language-kdl">'
        f"{highlighted}</code></pre></div>"
    )
