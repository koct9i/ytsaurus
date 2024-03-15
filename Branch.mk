##@ Git:

# map current branch upstream <remote>/<ANY> to <remote>/main
BRANCH_MAIN_BASE = $(shell git rev-parse --symbolic-full-name @{u} | sed -E 's#(refs/remotes/[^/]+)/(.*)#\1/main#')

# map current branch upstream <remote>/<ANY>/<VER> to <remote>/stable/<VER>
BRANCH_STABLE_BASE = $(shell git rev-parse --symbolic-full-name @{u} | sed -E 's#(refs/remotes/[^/]+)/(.*)/([^/]+)#\1/stable/\3#')

# map current branch upstream <remote>/<ANY>/<VER> to <remote>/pr/stable/<VER> <remote>/pr/<ANY>/<VER>
BRANCH_FRAGMENT_PREFIXES = $(shell git rev-parse --symbolic-full-name @{u} | sed -E 's#(refs/remotes/[^/]+)/(.*)/([^/]+)#\1/pr/stable/\3 \1/pr/\2/\3#')

# map current branch upstream <remote>/<ANY>/<VER> to all branches with prefixes <remote>/pr/stable/<VER> <remote>/pr/<ANY>/<VER>
BRANCH_FRAGMENTS = $(shell git for-each-ref --sort="-authordate" "--format=%(refname)" ${BRANCH_FRAGMENT_PREFIXES})

cherry-pick-branch-fragments: ## Reset current branch/version to stable/version and cherry-pick commits from fragments pr/stable/version and pr/branch/version
	git diff --quiet
	git diff --quiet --cached
	git reset --hard ${BRANCH_STABLE_BASE}
	git cherry-pick -x --keep-redundant-commits ${BRANCH_FRAGMENTS} --not HEAD

cherry-pick-main-branch-fragments: ## Reset current branch/version to main and cherry-pick commits from fragments pr/branch/version
	git diff --quiet
	git diff --quiet --cached
	git reset --hard ${BRANCH_MAIN_BASE}
	git cherry-pick -x --keep-redundant-commits ${BRANCH_FRAGMENTS} --not HEAD
