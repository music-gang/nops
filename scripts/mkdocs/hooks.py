"""MkDocs hooks: build the docs site from the Markdown of the repository as it is.

The pages keep their relative links, so GitHub and the tests in docs/ read them
unchanged. Here the root README becomes the home page and every link is
rewritten for the site: to another page of the site, or to GitHub when the
target is not on it (examples/, LICENSE, docs/archive/...).
"""

import os
import posixpath
import re

from mkdocs.structure.files import File

REPO = "https://github.com/music-gang/nops"
BRANCH = "main"

FENCE = re.compile(r"^\s*(```|~~~)")
INLINE_CODE = re.compile(r"(`[^`]*`)")
# [text](target) and src="target" / href="target" in the HTML of the README.
TARGET = re.compile(r"(\]\()([^)\s]+)|((?:src|href)=\")([^\"]+)")


def on_files(files, config):
    """Add the root README as the home page: docs/README.md is the index of
    the pages on GitHub and is left out of the site (exclude_docs)."""
    root = os.path.dirname(config.config_file_path)
    files.append(File.generated(config, "index.md", abs_src_path=os.path.join(root, "README.md")))
    return files


def on_page_markdown(markdown, page, config, files):
    root = os.path.dirname(config.config_file_path)
    origin = posixpath.normpath(os.path.relpath(page.file.abs_src_path, root).replace(os.sep, "/"))
    base = posixpath.dirname(origin)

    def rewrite(match):
        head, target = (match.group(1), match.group(2)) if match.group(1) else (match.group(3), match.group(4))
        return head + _target(target, base, page.file.src_uri, root, files)

    out, in_fence = [], False
    for line in markdown.split("\n"):
        if FENCE.match(line):
            in_fence = not in_fence
        elif not in_fence:
            # Odd items of the split are inline code: left as they are.
            parts = INLINE_CODE.split(line)
            line = "".join(p if i % 2 else TARGET.sub(rewrite, p) for i, p in enumerate(parts))
        out.append(line)
    return "\n".join(out)


def _target(target, base, page_uri, root, files):
    if "://" in target or target.startswith(("#", "mailto:")):
        return target
    path, hash_, fragment = target.partition("#")
    repo_path = posixpath.normpath(posixpath.join(base, path))

    # Both READMEs stand for the home page.
    if repo_path in ("README.md", "docs/README.md"):
        uri = "index.md"
    else:
        uri = repo_path.removeprefix("docs/") if repo_path.startswith("docs/") else None
    target_file = files.get_file_from_path(uri) if uri else None
    if target_file and not target_file.inclusion.is_excluded():
        rel = posixpath.relpath(uri, posixpath.dirname(page_uri) or ".")
        return rel + hash_ + fragment

    kind = "tree" if os.path.isdir(os.path.join(root, repo_path)) else "blob"
    return f"{REPO}/{kind}/{BRANCH}/{repo_path}{hash_}{fragment}"
