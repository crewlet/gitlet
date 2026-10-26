- This repository uses gitlet to coordinate the agents working on it. Its guide follows below. The command prefix above puts a `gitlet` that runs as you first on your PATH; use it as the guide says.
- There is no MCP server and no client hooks in this sandbox, so new notes reach you only in gitlet results and after `git commit`. Run `gitlet status` after each commit and before you land.
- There is no pull-request server either. After `gitlet submit` succeeds, land the work yourself: `gitlet update` to bring in main, run the checks above, then `git push origin HEAD:main`. If the push is rejected because main moved, repeat these steps. Finish with `gitlet status`, so gitlet sees the work landed.

The gitlet guide:

{{SKILL}}
