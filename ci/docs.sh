#!/usr/bin/env bash
# Publish docs/ to the gh-pages branch under /<TAGNAME>/ (and refresh /latest/).
# Runs inside GitHub Actions — expects GITHUB_WORKSPACE, GITHUB_TOKEN,
# GITHUB_REPOSITORY in the environment.
#
# Usage: ci/docs.sh <TAGNAME>
#   <TAGNAME> is "latest" on main pushes, or the release tag on releases.

set -e

TAGNAME=$1
if [ -z "$TAGNAME" ]; then
  echo "usage: $0 <tagname>" >&2
  exit 2
fi

GH_REF=github.com/aqa-alex/selenwright-gridlane.git

git config user.name  "${GITHUB_REPOSITORY}"
git config user.email "github-actions[bot]@users.noreply.github.com"
git remote add upstream "https://${GITHUB_TOKEN}@${GH_REF}"
git fetch upstream

echo "Deleting old output"
rm -rf "${GITHUB_WORKSPACE}/docs/output"
mkdir "${GITHUB_WORKSPACE}/docs/output"
git worktree prune
rm -rf "${GITHUB_WORKSPACE}/.git/worktrees/docs/output/"

echo "Checking out gh-pages branch into docs/output"
if git rev-parse --verify upstream/gh-pages >/dev/null 2>&1; then
  git worktree add -B gh-pages "${GITHUB_WORKSPACE}/docs/output" upstream/gh-pages
else
  echo "gh-pages branch does not exist yet — creating orphan"
  git worktree add --detach "${GITHUB_WORKSPACE}/docs/output"
  cd "${GITHUB_WORKSPACE}/docs/output"
  git checkout --orphan gh-pages
  git rm -rf . >/dev/null 2>&1 || true
  cd "${GITHUB_WORKSPACE}"
fi

echo "Removing existing files for ${TAGNAME}"
mkdir -p "${GITHUB_WORKSPACE}/docs/output/${TAGNAME}"
rm -rf "${GITHUB_WORKSPACE}/docs/output/${TAGNAME}"/*

echo "Generating docs for ${TAGNAME}"
docker run --rm \
  -v "${GITHUB_WORKSPACE}/docs/:/documents/" \
  --name asciidoc-to-html \
  asciidoctor/docker-asciidoctor \
  asciidoctor -a "revnumber=${TAGNAME}" \
              -D "/documents/output/${TAGNAME}" \
              index.adoc

if [ "${TAGNAME}" != "latest" ]; then
  echo "Refreshing latest/ alias"
  mkdir -p "${GITHUB_WORKSPACE}/docs/output/latest"
  rm -rf "${GITHUB_WORKSPACE}/docs/output/latest"/*
  cp -R "${GITHUB_WORKSPACE}/docs/output/${TAGNAME}/." "${GITHUB_WORKSPACE}/docs/output/latest/"
fi

echo "Updating gh-pages branch"
cd "${GITHUB_WORKSPACE}/docs/output"
git add --all
if git diff --cached --quiet; then
  echo "No doc changes to publish"
  exit 0
fi
git commit -m "Publishing to gh-pages (${TAGNAME})"
git push upstream HEAD:gh-pages
