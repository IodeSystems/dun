# node_query — cheat sheet

> CSS-inspired selector language over the project tree.
> Full grammar: `node_query(selector: "?")`.

## Core pattern: `path=<file> <tag>`

A bare path is NOT a selector — scope it with `path=`:

```
path=cmd/dun/tui.go func                # ✓ what's in a file
path=cmd/dun/tui.go                     # ✓ the file itself
cmd/dun/tui.go                          # ✗ parsed as a type name
```

## Finding symbols

```
#Save                                  # find by name anywhere
#'harness.go#Save'                     # pin to one file
func name~=^Test                       # regex on name (~=)
func:not([name^=Test])                 # negation
```

## Call graph (::in / ::out)

```
#'harness.go#Start'::in.call > *       # who calls Start
#'main'::out.call > *                  # what main calls
func:empty(::in)                       # dead code (no callers)
```

## Text search (::grep)

```
path=harness.go ::grep('-E Server|Harness')   # regex lines
path=plan/plan.md ::grep('sub-agent')          # literal in markdown
path=*.go ::grep('-w TODO')                    # across all .go files
```

ONE quoted arg — no inner quotes. Flags: `-E` (regex), `-w` (word), `-i` (case), `-A/-B/-C<n>` (context).

## Markdown headings as structured nodes

Markdown files have a heading tree. Headings are queryable like any other symbol:

```
path=plan/plan.md > *                   # all children (headings)
path=plan/plan.md > * > *               # two levels deep
```

The `in` field shows the full hierarchy: `plan/plan.md#dun — plan.Active work.✅ D  Slice 5`.

Use this to navigate docs, plans, and specs — then `node_read` the section.

## File structure

```
path=subagent.go > *                    # top-level symbols
path=subagent.go method                 # only methods
path=subagent.go func                   # only functions
```

## Attributes and brackets

```
path=a/b.go ≡ [path=a/b.go]            # brackets optional
func[name=Save]                         # exact match
func[name^=Test]                        # prefix
func[name$=Handler]                     # suffix
func[name*=Cache]                       # contains
```

## Union and filtering

```
func,method                             # union: funcs OR methods
func,path=a.go                          # filter: funcs in a.go
func [name=a|name=b]                    # OR in brackets
```

## Common mistakes

1. **Bare path** → `cmd/dun/tui.go` is parsed as a type, not a file. Use `path=cmd/dun/tui.go`.
2. **Double-quoted grep** → `::grep('"pattern"')` matches nothing. Use `::grep('pattern')`.
3. **Space = node boundary** → `func path=a.go` means "things inside a func", not "func in a.go". Use `func,path=a.go` or `path=a.go func`.
4. **`*` never matches edges** → `::out` and `::grep` results need explicit naming: `#'x'::out.call > *`.

## Recipes

```
# callers of a function, only in one file
#Start::in.call > * path=cmd/dun/main.go

# endpoints (imports huma, calls Get/Post)
import#huma::in.call::grep('-E (Get|Post)\(')

# tour the project
:root > *
```
