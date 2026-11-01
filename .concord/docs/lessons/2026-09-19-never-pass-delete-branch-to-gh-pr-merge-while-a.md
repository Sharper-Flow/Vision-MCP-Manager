Symptom: after `gh pr merge <n> --rebase --delete-branch`, all Concord operations return transport_failure with 'concord project-resolve: store: resolve_project: git_unreachable: signed directory/worktree is not a git repository'. concord_work_start on the same work item returns 'cannot resolve managed worktree ...: ENOENT'. Moving the session to another item's worktree also fails if that item is terminal ('cannot resume terminal work item').

Cause: the Concord worktree is a plain git worktree at a path Concord records, with no marker file inside it. Deleting the branch deletes the worktree, so the recorded path stops resolving and the session is stranded.

Recovery, from the main checkout on the default branch:
  git worktree add -b <branch> <recorded-worktree-path> <merged-commit>
Then call concord_work_start with the work_id. It re-binds the session and the workflow resumes at its existing version. No workflow state is lost and the merge is never at risk.

Prevention: merge with `gh pr merge <n> --rebase` and no --delete-branch. Let Concord reclaim the worktree via concord_work_transition.worktree_reclaim once the item is terminal. worktree_reclaim also refuses when another live session occupies the worktree, a check plain branch deletion does not perform.