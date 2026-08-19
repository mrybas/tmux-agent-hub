Another AI agent just finished a task in {{cwd}}. Its own summary:

{{output}}

Do not trust the summary — review the actual changes: run
`git -C {{cwd}} status` and `git -C {{cwd}} diff HEAD`, read the touched
files, and report bugs, risks and missing tests.
