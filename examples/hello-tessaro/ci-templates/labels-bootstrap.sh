#!/usr/bin/env bash
# Create the reserved SDLC + deploy label vocabulary in a GitHub repo, once.
# The loop can only APPLY labels that already exist. Run against your app repo:
#
#   GH_REPO=owner/hello-tessaro ./ci-templates/labels-bootstrap.sh
#
# (Mirrors sdlc.ReservedLabels() in coreutils/pkg/sdlc/labels.go — keep in sync.)
set -euo pipefail
REPO="${GH_REPO:?set GH_REPO=owner/name}"

create() { gh label create "$1" --repo "$REPO" --color "$2" --description "$3" --force; }

# sdlc:* — private lifecycle state machine (applied by the conductor)
create "sdlc:go"          "0e8a16" "Bless this issue: conductor may start a round"
create "sdlc:in-progress" "fbca04" "Conductor is working this issue (do not double-pick)"
create "sdlc:qa"          "5319e7" "In QA / review"
create "sdlc:approved"    "0e8a16" "Approved, ready to deploy"
create "sdlc:blocked"     "b60205" "Do not pick up"
create "sdlc:ignore"      "cccccc" "Never pick up"
create "sdlc:done"        "1d76db" "Resolved and deployed"

# deploy:* — the public deploy baton (adding one triggers .github/workflows/deploy.yml)
create "deploy:dev"       "c2e0c6" "Deploy to dev"
create "deploy:qa"        "c2e0c6" "Deploy to qa"
create "deploy:prod"      "d93f0b" "Deploy to prod"

# priority / type — scheduling + triage (optional but recommended)
for p in p0 p1 p2 p3; do create "priority:$p" "d4c5f9" "Scheduling priority $p"; done
for t in bug enhancement task docs chore; do create "type:$t" "bfdadc" "Type: $t"; done

echo "bootstrapped reserved labels in $REPO"
