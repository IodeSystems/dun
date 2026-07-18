# Example dun tool-result renderer (Starlark).
#
# Drop this in ~/.dun/renderers/ (or $DUN_HOME/renderers/) and it OVERRIDES the
# built-in "search" renderer. Scripts load at TUI startup, sorted by filename;
# the last registration for a tool wins.
#
# A renderer is a function ctx -> (preview, full):
#   ctx["tool"]    the tool name (str)
#   ctx["args"]    the call arguments (dict)
#   ctx["result"]  the raw result (str; often JSON text)
#   ctx["width"]   the current viewport width (int)
# Return (preview, full): preview is the one-line collapsed form, full is the
# expanded body. Return a single string for "full only, generic preview".
#
# Predeclared helpers:
#   renderer(tool, fn)   register fn for a tool
#   dim(s) / tool(s) / bold(s)   ANSI styling
#   diff(s)              colorize a unified diff
#   clip(s, n)           truncate to n runes with an ellipsis
#   json.decode/encode/indent

def render(ctx):
    # raglit search results are commonly a JSON array of {title, score, line}.
    hits = json.decode(ctx["result"])
    if type(hits) != "list":
        return None  # not the shape we expect → fall back to generic

    lines = []
    for h in hits:
        title = h.get("title", "?")
        score = h.get("score", 0.0)
        snippet = clip(h.get("line", ""), 80)
        # NB: Starlark's % operator has no width/precision (no %.2f) — keep verbs bare.
        lines.append("%s  %s\n    %s" % (bold(title), dim("(%s)" % score), dim(snippet)))

    preview = dim("  → %d result(s)" % len(hits))
    return preview, "\n".join(lines)

renderer("search", render)
