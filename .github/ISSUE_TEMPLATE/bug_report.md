---
name: Bug report
about: Report something that isn't working correctly
labels: bug
---

**Describe the bug**
A clear description of what went wrong.

**To reproduce**
Steps or a minimal code snippet that triggers the issue.

**Expected behavior**
What you expected to happen.

**Debug logs**
Run `dexter init --force` in your project root to rebuild the index. If the bug is still there, enable debug mode (see the [Debugging section](https://github.com/remoteoss/dexter#debugging) in the README), make the bug happen again, and include the relevant lines of the daemon log. `dexter log` in your project prints the path of that log. You can also include your editor's LSP log.

<details>
<summary>Logs</summary>

```
Paste debug logs here
```

</details>

**Environment**
- Dexter version (`dexter version`):
- Elixir version (`elixir --version`):
- Editor and version:
- OS:

**Did you try `dexter init --force`?**
Yes / No
