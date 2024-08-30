DOCKER_OVERRIDE_BASE_REPOSITORY ?= ghcr.io/ytsaurus/ytsaurus
DOCKER_OVERRIDE_BASE_IMAGE ?= stable-23.2.1-relwithdebinfo

##@ Releases:

YT_VERSION ?= 24.1

BRANCH_PREFIX = mirror-ytsaurus
# (refs/remotes/<remote=origin>)/mirror-ytsaurus/(bootstrap|patches|releases)/stable/(<version>)(EOS or /<patch>)
BRANCH_NAME_PATTERN = (refs/remotes/[^/]+)/${BRANCH_PREFIX}/([^/]+)/stable/([^/]+)($$|/.*)
UPSTREAM = $(shell git rev-parse --symbolic-full-name @{u})

BRANCH_STABLE_BASE = $(shell echo ${UPSTREAM} | sed -E 's#${BRANCH_NAME_PATTERN}#\1/stable/\3#')
REMOTE_BOOTSTRAP_BRANCH = $(shell echo ${UPSTREAM} | sed -E 's#${BRANCH_NAME_PATTERN}#\1/${BRANCH_PREFIX}/bootstrap/stable/\3#')
LOCAL_BOOTSTRAP_BRANCH = $(shell echo ${UPSTREAM} | sed -E 's#${BRANCH_NAME_PATTERN}#${BRANCH_PREFIX}/bootstrap/stable/\3#')
RELEASE_BRANCH = $(shell echo ${UPSTREAM} | sed -E 's#${BRANCH_NAME_PATTERN}#${BRANCH_PREFIX}/releases/stable/\3#')
BRANCH_FRAGMENT_PREFIXES = $(shell echo ${UPSTREAM} | sed -E 's#${BRANCH_NAME_PATTERN}#\1/${BRANCH_PREFIX}/patches/stable/\3#')
BRANCH_FRAGMENTS = $(shell git for-each-ref --sort="-authordate" "--format=%(refname)" ${REMOTE_BOOTSTRAP_BRANCH}  ${BRANCH_FRAGMENT_PREFIXES})

checkout-bootstrap-branch: ## Checkout bootstrap branch for the current release.
	git diff --quiet
	git diff --quiet --cached
	git checkout ${LOCAL_BOOTSTRAP_BRANCH}

checkout-release-branch: ## Checkout release branch for the current release.
	git diff --quiet
	git diff --quiet --cached
	git checkout ${RELEASE_BRANCH}

define newline


endef

rebuild-release-branch: checkout-release-branch ## Rebuild release branch by resetting it to the corresponding stable branch and cherry-picking fragments from mirror-ytsaurus/patches/stable/version/*.
	git reset --hard ${BRANCH_STABLE_BASE}
	$(foreach fragment,${BRANCH_FRAGMENTS},git cherry-pick -x --keep-redundant-commits ${fragment} --not HEAD $(newline))

push-release-branch: TAG_NAME=tracto-${YT_VERSION}-$(shell git show -s --pretty=%cs-%H ${RELEASE_BRANCH})
push-release-branch: ## Push release branch with corresponding tag. Should start CI.
	git tag -a ${TAG_NAME} -m "YTsaurus tracto ${YT_VERSION} release" ${RELEASE_BRANCH}
	git push -f origin ${RELEASE_BRANCH}
	git push origin ${TAG_NAME}

release-image: DOCKER_IMAGE_TAG=tracto-${YT_VERSION}-$(shell git show -s --pretty=%cs-%H)
release-image: hack-local-python docker-ytsaurus ## Build release docker image and push to nemax registry.
