# CLI usage

[Back to README](../README.md)

```bash
rein spec kubectl                  # learn the tool once, cache the result
rein in kubectl "which pods are crashlooping in staging?"
rein in gh "how many open PRs are assigned to me?"
rein list                          # what has been learned so far
```

The `in` is optional whenever the next word is a tool on your PATH, so the
short form works too:

```bash
rein ffmpeg "make a 3 second test video"
```

Rein's own commands (`spec`, `list`, `help`) always take precedence, so a
tool that happens to share one of those names needs the explicit `rein in`.

`rein in` discovers the tool automatically on first use, so `rein spec`
is only needed to pre-warm the cache or re-learn after an upgrade
(`rein in --refresh`).

Rein's own flags work before or after the intent. Any *other* dashed word
stays inside the intent, so you can ask about the wrapped tool's flags without
them being parsed away:

```bash
rein in --auto gh "sync my forks"
rein in gh "sync my forks" --auto            # identical
rein in gh "which commands support --json?"  # --json stays in the intent
rein in gh -- "what does --auto do?"         # "--" ends flag parsing
```
