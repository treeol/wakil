#!/usr/bin/env python3
"""
Google Search MCP Server
========================
A Model Context Protocol server that exposes Google Search (via Google's Custom
Search JSON API) plus a URL reader that fetches a page and returns its readable
text.

Requires:
    pip install mcp

Environment variables:
    GOOGLE_API_KEY  - API key from Google Cloud Console
    GOOGLE_CX       - Search Engine ID from Programmable Search Engine

Usage:
    python google_search_mcp.py
"""

import os
import re
import json
import datetime
import urllib.request
import urllib.parse
from html.parser import HTMLParser
from mcp.server.fastmcp import FastMCP


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

MCP = FastMCP("Google Search", json_response=True)

GOOGLE_API_KEY = os.getenv("GOOGLE_API_KEY", "")
GOOGLE_CX = os.getenv("GOOGLE_CX", "")

USER_AGENT = "GoogleSearchMCP/1.0"
MAX_DOWNLOAD_BYTES = 2_000_000  # cap each fetch at ~2 MB


def _validate():
    """Raise if API credentials are missing."""
    if not GOOGLE_API_KEY:
        raise RuntimeError("GOOGLE_API_KEY environment variable is not set.")
    if not GOOGLE_CX:
        raise RuntimeError("GOOGLE_CX (Programmable Search Engine ID) is not set.")


# ---------------------------------------------------------------------------
# HTML -> text helpers (stdlib only)
# ---------------------------------------------------------------------------

def _collapse_ws(s: str) -> str:
    """Collapse runs of whitespace while keeping paragraph breaks readable."""
    s = re.sub(r"[ \t\f\v\r]+", " ", s)
    s = re.sub(r" *\n *", "\n", s)
    s = re.sub(r"\n{3,}", "\n\n", s)
    return s.strip()


class _TextExtractor(HTMLParser):
    """Pull visible text (and the <title>) out of an HTML document."""

    _SKIP = {"script", "style", "noscript", "template", "svg"}
    _BLOCK = {
        "p", "div", "br", "li", "tr", "section", "article", "header", "footer",
        "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "table", "blockquote",
    }

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self._skip = 0
        self._in_title = False
        self.title_parts: list[str] = []
        self.parts: list[str] = []

    def handle_starttag(self, tag, attrs):
        if tag in self._SKIP:
            self._skip += 1
        elif tag == "title":
            self._in_title = True
        elif tag in self._BLOCK:
            self.parts.append("\n")

    def handle_endtag(self, tag):
        if tag in self._SKIP and self._skip > 0:
            self._skip -= 1
        elif tag == "title":
            self._in_title = False
        elif tag in self._BLOCK:
            self.parts.append("\n")

    def handle_data(self, data):
        if self._skip:
            return
        if self._in_title:
            self.title_parts.append(data)
        else:
            self.parts.append(data)


def _html_to_text(html: str) -> tuple[str, str]:
    """Return (title, text) extracted from an HTML string."""
    parser = _TextExtractor()
    try:
        parser.feed(html)
    except Exception:
        # Malformed markup: fall back to a crude tag strip.
        return "", _collapse_ws(re.sub(r"<[^>]+>", " ", html))
    return _collapse_ws("".join(parser.title_parts)), _collapse_ws("".join(parser.parts))


def _charset_from(content_type: str) -> str:
    if "charset=" in content_type:
        cs = content_type.split("charset=")[-1].split(";")[0].strip().strip('"')
        if cs:
            return cs
    return "utf-8"


# ---------------------------------------------------------------------------
# Date helpers
# ---------------------------------------------------------------------------

def _parse_date(s: str, end: bool) -> str:
    """Convert a flexible date string to YYYYMMDD for the Google sort parameter.

    Accepts YYYY, YYYY-MM, or YYYY-MM-DD (slashes also accepted).
    When end=True, incomplete dates are filled to the last day of the period
    (e.g. "2023" → "20231231", "2023-06" → "20230630").
    """
    s = s.strip().replace("/", "-")
    parts = s.split("-")
    if len(parts) == 1:
        year = int(parts[0])
        return f"{year}1231" if end else f"{year}0101"
    if len(parts) == 2:
        year, month = int(parts[0]), int(parts[1])
        if end:
            # last day of the month
            last = (datetime.date(year, month % 12 + 1, 1) - datetime.timedelta(days=1)).day \
                   if month < 12 else 31
            return f"{year}{month:02d}{last:02d}"
        return f"{year}{month:02d}01"
    if len(parts) == 3:
        year, month, day = int(parts[0]), int(parts[1]), int(parts[2])
        return f"{year}{month:02d}{day:02d}"
    raise ValueError(f"Unrecognized date format: {s!r}")


# ---------------------------------------------------------------------------
# Tools
# ---------------------------------------------------------------------------

@MCP.tool()
def google_search(
    query: str,
    num: int = 5,
    start: int = 1,
    after: str = "",
    before: str = "",
) -> list[dict]:
    """Search Google using the Custom Search JSON API.

    Returns ranked results as metadata only (title, url, snippet); it does not
    fetch the pages. Use fetch_url to read a result's full content.

    Args:
        query: The search query.
        num: Number of results to return (1-10, default 5).
        start: Pagination offset (1-based, default 1).
        after: Restrict results to pages published on or after this date.
            Accepts YYYY, YYYY-MM, or YYYY-MM-DD (e.g. "2022", "2022-06",
            "2022-06-15"). When either after or before is set the results are
            sorted by date; leave both empty to use the default 3-month window
            with relevance-based ranking.
        before: Restrict results to pages published on or before this date.
            Same format as `after`.

    Returns:
        List of dicts with keys: title, url, snippet.
    """
    _validate()

    # Clamp values
    num = max(1, min(10, num))
    start = max(1, start)

    base_url = "https://www.googleapis.com/customsearch/v1"
    p: dict = {
        "key": GOOGLE_API_KEY,
        "cx": GOOGLE_CX,
        "q": query,
        "num": num,
        "start": start,
    }

    if after or before:
        # Explicit range → sort=date:r:... (absolute window, date-ordered)
        after_s = _parse_date(after, end=False) if after else "19900101"
        before_s = _parse_date(before, end=True) if before else \
                   datetime.date.today().strftime("%Y%m%d")
        p["sort"] = f"date:r:{after_s}:{before_s}"
    else:
        # Default: last 3 months, relevance-sorted (dateRestrict keeps ranking intact)
        p["dateRestrict"] = "m3"

    params = urllib.parse.urlencode(p)

    url = f"{base_url}?{params}"

    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})

    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        raise RuntimeError(f"Google API error ({e.code}): {body}")
    except urllib.error.URLError as e:
        raise RuntimeError(f"Network error: {e.reason}")

    items = data.get("items", [])
    return [
        {
            "title": item.get("title", ""),
            "url": item.get("link", ""),
            "snippet": item.get("snippet", ""),
        }
        for item in items
    ]


@MCP.tool()
def fetch_url(url: str, max_chars: int = 5000) -> dict:
    """Fetch a URL and return its readable text content with HTML stripped.

    Use this to read the full content of a page found via google_search, rather
    than relying on the short search snippet.

    Args:
        url: The page URL to fetch (must be http:// or https://).
        max_chars: Maximum characters of text to return (default 5000).

    Returns:
        Dict with keys: url, title, text, truncated.
    """
    if not url.lower().startswith(("http://", "https://")):
        raise RuntimeError("url must start with http:// or https://")
    max_chars = max(100, max_chars)

    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            content_type = resp.headers.get("Content-Type", "")
            raw = resp.read(MAX_DOWNLOAD_BYTES)
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"HTTP error ({e.code}) fetching {url}")
    except urllib.error.URLError as e:
        raise RuntimeError(f"Network error fetching {url}: {e.reason}")

    text_body = raw.decode(_charset_from(content_type), errors="replace")

    looks_html = "html" in content_type.lower() or "<html" in text_body[:2000].lower()
    if looks_html:
        title, text = _html_to_text(text_body)
    else:
        title, text = "", _collapse_ws(text_body)

    truncated = len(text) > max_chars
    if truncated:
        text = text[:max_chars].rstrip() + "…"

    return {"url": url, "title": title, "text": text, "truncated": truncated}


# ---------------------------------------------------------------------------
# Entry Point
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    # stdio transport is the default — works with Claude, Cursor, VS Code, etc.
    MCP.run()
