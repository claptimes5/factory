#!/bin/sh
# Create the labels the factory uses. Needs the gh CLI, authenticated.
# `factory setup` does the same without gh; keep the two in step
# (pipeline.Labels).
set -eu
repo="${1:-claptimes5/factory}"
gh label create "factory:ready"        -R "$repo" -c 0e8a16 -d "Factory: pick this up" --force
gh label create "factory:running"      -R "$repo" -c fbca04 -d "Factory: a job is running" --force
gh label create "factory:parked"       -R "$repo" -c d93f0b -d "Factory: needs a human; remove to retry" --force
gh label create "factory:awaiting-you" -R "$repo" -c 1d76db -d "Factory: draft PR ready for your review" --force
gh label create "factory:retry"        -R "$repo" -c 5319e7 -d "Factory: send a parked issue back to triage" --force
gh label create "factory:epic"         -R "$repo" -c c5def5 -d "Factory: split into child issues" --force
