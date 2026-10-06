#!/usr/bin/env python3
"""Verify the release CI gate (issue #56) fails closed on every publish path.

Two layers of regression:

1. Structure — release.yml wires `ci_gate` into every publication side effect
   (publish_* jobs and create_release), the required-job list matches ci.yml,
   and no publish job can bypass the gate with `always()`. The immutable
   release SHA is pinned once in `prepare` (issue #56 hardening) and EVERY
   bind-release-ref call site (fan-out dry legs, gate, publish fan-in) carries
   that pin.
2. Behavior — the actual gate shell embedded in release.yml is extracted and
   executed against stubbed GitHub API fixtures in a throwaway git repo:
   only a CI run at the EXACT release commit with every required job green
   passes; missing / pending / skipped / cancelled / failed checks, runs on
   another commit, a tag moved after the pin, and API failures all block
   publication. The bind-release-ref shell is likewise extracted and run
   against temp tagged repos where the tag moves after the pin — it must fail
   closed BEFORE the checkout, while non-publish dry-runs stay untouched.
"""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap

REQUIRED_JOBS = ["backend", "frontend", "e2e", "mariadb-contract", "helm", "docker", "actionlint"]
PUBLISH_JOBS = ["publish_aio", "publish_split", "publish_probe", "publish_chart", "create_release"]
GATE_STEP = "      - name: Require successful CI for the exact release commit"
BIND_ACTION = "uses: ./.github/actions/bind-release-ref"
# Every job that binds to the release tag must enforce the SHA pinned in
# prepare: dry legs + ci_gate read it from needs.prepare (fan-out), the
# publish/create_release fan-in reads the dry_run pass-through.
BIND_JOBS = {
    "ci_gate": "prepare",
    "dry_binaries": "prepare",
    "dry_chart": "prepare",
    "dry_images": "prepare",
    "publish_aio": "dry_run",
    "publish_split": "dry_run",
    "publish_probe": "dry_run",
    "publish_chart": "dry_run",
    "create_release": "dry_run",
}


def job_ids(workflow: Path):
    """Top-level job ids of a workflow (2-space keys after the `jobs:` key)."""
    lines = workflow.read_text().splitlines()
    start = lines.index("jobs:")
    ids = []
    for line in lines[start + 1:]:
        match = re.match(r"^  ([a-z0-9_-]+):\s*$", line)
        if match:
            ids.append(match.group(1))
    return ids


def job_block(lines, job_id):
    """Slice of a workflow's lines belonging to one job."""
    start = lines.index(f"  {job_id}:")
    end = start + 1
    while end < len(lines) and not re.match(r"^  [a-z0-9_-]+:\s*$", lines[end]):
        end += 1
    return lines[start:end]


def bind_expected_sha(block):
    """expected_sha input value of a job block's bind-release-ref call, if any."""
    for index, line in enumerate(block):
        if BIND_ACTION not in line:
            continue
        for follow in block[index + 1:]:
            if follow.strip() == "with:":
                continue
            if not follow.startswith("          "):
                return None
            if follow.strip().startswith("expected_sha:"):
                return follow.strip()
        return None
    return None


def structural_checks(root: Path):
    release = root / ".github/workflows/release.yml"
    ci = root / ".github/workflows/ci.yml"
    action = root / ".github/actions/bind-release-ref/action.yml"
    lines = release.read_text().splitlines()

    if "ci_gate" not in job_ids(release):
        raise SystemExit("release.yml is missing the ci_gate job")
    gate = job_block(lines, "ci_gate")
    needs = [line.strip() for line in gate if line.strip().startswith("needs:")]
    if needs != ["needs: prepare"]:
        raise SystemExit(f"ci_gate must need only prepare, got {needs}")

    for job in PUBLISH_JOBS:
        block = job_block(lines, job)
        need_lines = [line.strip() for line in block if line.strip().startswith("needs:")]
        if len(need_lines) != 1 or "ci_gate" not in need_lines[0]:
            raise SystemExit(f"{job} must list ci_gate in needs (fail closed), got {need_lines}")
        ifs = [line.strip() for line in block if line.strip().startswith("if:")]
        if any("always()" in line for line in ifs):
            raise SystemExit(f"{job} must not bypass skipped needs with always()")
    create_if = "\n".join(job_block(lines, "create_release"))
    if "needs.ci_gate.result == 'success'" not in create_if:
        raise SystemExit("create_release must require needs.ci_gate.result == 'success'")

    # The gate's required-job list must track the CI workflow's real jobs.
    workflow = "\n".join(lines)
    match = re.search(r"required_jobs=\(([^)]*)\)", workflow)
    if not match:
        raise SystemExit("release.yml gate is missing the required_jobs list")
    declared = sorted(match.group(1).split())
    if declared != sorted(job_ids(ci)):
        raise SystemExit(
            f"gate required_jobs {declared} does not match ci.yml jobs {sorted(job_ids(ci))}"
        )

    gate_script = extract_gate_script(lines)
    if "${{" in gate_script:
        raise SystemExit("gate script must never interpolate GitHub expressions into shell")

    # Issue #56 hardening: the release source SHA is pinned ONCE in prepare and
    # every mutable-tag re-resolution is compared against that pin.
    prep = "\n".join(job_block(lines, "prepare"))
    if "id: pin" not in prep or "source_sha: ${{ steps.pin.outputs.source_sha }}" not in prep:
        raise SystemExit("prepare must resolve the immutable source SHA once (steps.pin -> source_sha)")
    dry = "\n".join(job_block(lines, "dry_run"))
    if "source_sha: ${{ needs.prepare.outputs.source_sha }}" not in dry:
        raise SystemExit("dry_run must pass the pinned source_sha through to the publish fan-in")
    if "EXPECTED_SHA" not in gate_script:
        raise SystemExit("gate script must verify the tag against the SHA pinned in prepare")
    if "  expected_sha:" not in action.read_text():
        raise SystemExit("bind-release-ref must declare the expected_sha input")

    sites = [index for index, line in enumerate(lines) if BIND_ACTION in line]
    if len(sites) != len(BIND_JOBS):
        raise SystemExit(
            f"expected {len(BIND_JOBS)} bind-release-ref call sites, found {len(sites)}"
        )
    for job, source in BIND_JOBS.items():
        want = f"expected_sha: ${{{{ needs.{source}.outputs.source_sha }}}}"
        got = bind_expected_sha(job_block(lines, job))
        if got != want:
            raise SystemExit(f"{job} bind must pass {want}, got {got}")
    return gate_script


def extract_gate_script(lines):
    header = lines.index(GATE_STEP)
    start = lines.index("        run: |", header) + 1
    end = start
    while end < len(lines) and (not lines[end] or lines[end].startswith("          ")):
        end += 1
    script = textwrap.dedent("\n".join(lines[start:end]))
    if "set -euo pipefail" not in script or "required_jobs=" not in script:
        raise SystemExit("gate step is missing its required shell contract")
    return script


def extract_bind_script(action: Path):
    """Shell body of the bind-release-ref composite action."""
    lines = action.read_text().splitlines()
    start = lines.index("      run: |") + 1
    end = start
    while end < len(lines) and (not lines[end] or lines[end].startswith("        ")):
        end += 1
    script = textwrap.dedent("\n".join(lines[start:end]))
    if "set -euo pipefail" not in script or "EXPECTED_SHA" not in script:
        raise SystemExit("bind-release-ref script is missing its required shell contract")
    return script


def git(repo, *args):
    return subprocess.run(
        ["git", "-C", str(repo), *args], check=True, text=True, capture_output=True
    ).stdout.strip()


def fixture(directory: Path, runs, jobs_by_run):
    (directory / "runs.json").write_text(json.dumps({
        "total_count": len(runs), "workflow_runs": runs,
    }))
    for run_id, jobs in jobs_by_run.items():
        (directory / f"jobs-{run_id}.json").write_text(json.dumps({
            "total_count": len(jobs), "jobs": jobs,
        }))


def jobs_for(conclusions):
    """Job list from {name: conclusion}; None conclusion = still pending."""
    return [
        {"name": name, "status": "completed" if conclusion else "in_progress",
         "conclusion": conclusion}
        for name, conclusion in conclusions.items()
    ]


def all_green():
    return jobs_for({name: "success" for name in REQUIRED_JOBS})


def main():
    root = Path(__file__).resolve().parents[2]
    gate_script = structural_checks(root)
    print("PASS structure: ci_gate wired into every publish path, required_jobs tracks ci.yml")
    print("PASS structure: source SHA pinned once in prepare, all 9 bind sites carry it")

    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="phoenix-ci-gate-") as directory:
        temporary = Path(directory)

        # Throwaway repo: tag v9.9.9 on the release commit (== HEAD); its
        # parent is the "another commit" whose green checks must not satisfy.
        repo = temporary / "repo"
        subprocess.run(["git", "init", "-q", "-b", "main", str(repo)], check=True)
        git(repo, "config", "user.email", "gate-test@example.invalid")
        git(repo, "config", "user.name", "gate-test")
        for message in ("one", "two"):
            git(repo, "commit", "--allow-empty", "-q", "-m", message)
        release_sha = git(repo, "rev-parse", "HEAD")
        git(repo, "tag", "v9.9.9")
        ancestor_sha = git(repo, "rev-parse", "HEAD~1")

        fake_bin = temporary / "bin"
        fake_bin.mkdir()
        curl = fake_bin / "curl"
        curl.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "url=\"${*: -1}\"\n"
            "printf '%s\\n' \"${url}\" >> \"${CURL_LOG}\"\n"
            "if [ \"${CURL_FAIL:-0}\" = \"1\" ]; then exit 22; fi\n"
            "case \"${url}\" in\n"
            "  */workflows/ci.yml/runs*) cat \"${FIXTURE_DIR}/runs.json\"; exit 0 ;;\n"
            "  */actions/runs/*/jobs*)\n"
            "    id=\"$(printf '%s' \"${url}\" | sed -E 's#.*/actions/runs/([0-9]+)/jobs.*#\\1#')\"\n"
            "    if [ -f \"${FIXTURE_DIR}/jobs-${id}.json\" ]; then\n"
            "      cat \"${FIXTURE_DIR}/jobs-${id}.json\"; exit 0\n"
            "    fi\n"
            "    exit 22 ;;\n"
            "  *) exit 22 ;;\n"
            "esac\n"
        )
        curl.chmod(0o700)
        gate = temporary / "ci-gate.sh"
        gate.write_text(gate_script + "\n")

        def run(name, runs, jobs_by_run, expect_pass, publish="1", curl_fail="0",
                expected_sha=None):
            fixture_dir = temporary / (name + "-fixtures")
            fixture_dir.mkdir()
            fixture(runs=runs, jobs_by_run=jobs_by_run, directory=fixture_dir)
            curl_log = temporary / (name + "-curl.log")
            result = subprocess.run(
                ["bash", str(gate)], cwd=repo, text=True, capture_output=True, timeout=30,
                env={**os.environ, "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                     "PUBLISH": publish, "TAG_NAME": "v9.9.9",
                     "EXPECTED_SHA": release_sha if expected_sha is None else expected_sha,
                     "GITHUB_REPOSITORY": "fixture/repo", "GITHUB_TOKEN": "dummy-token",
                     "GITHUB_API_URL": "http://api.fixture.invalid",
                     "FIXTURE_DIR": str(fixture_dir), "CURL_LOG": str(curl_log),
                     "CURL_FAIL": curl_fail},
            )
            passed = result.returncode == 0
            if passed != expect_pass:
                raise SystemExit(
                    f"{name}: expected {'pass' if expect_pass else 'fail'}, "
                    f"got exit={result.returncode}\n{result.stdout}\n{result.stderr}"
                )
            secret = "dummy-token" in result.stdout + result.stderr
            if secret:
                raise SystemExit(f"{name}: gate echoed the API token")
            log = curl_log.read_text().splitlines() if curl_log.exists() else []
            print(f"PASS {name} exit={result.returncode} api_calls={len(log)}")
            return log, result

        log, _ = run("dry-run-not-gated", [], {}, expect_pass=True, publish="0")
        if log:
            raise SystemExit("dry-run gate must not query the CI API at all")

        # Dry-run stays unpinned too: the pin is a publish-only contract.
        log, _ = run("dry-run-ignores-missing-pin", [], {}, expect_pass=True,
                     publish="0", expected_sha="")
        if log:
            raise SystemExit("dry-run gate must not query the CI API at all")

        run("all-green-exact-sha",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "success"}],
            {101: all_green()}, expect_pass=True)

        run("missing-ci-run-fails", [], {}, expect_pass=False)

        for conclusion in ("failure", "cancelled", "skipped", None):
            run(f"workflow-{conclusion}-blocks-even-with-green-jobs",
                [{"id": 101, "head_sha": release_sha,
                  "status": "completed" if conclusion else "in_progress",
                  "conclusion": conclusion}],
                {101: all_green()}, expect_pass=False)

        # A successful run envelope never substitutes for inspecting its jobs.
        for conclusion in ("failure", "cancelled", "skipped", None):
            run(f"job-{conclusion}-blocks-despite-green-envelope",
                [{"id": 101, "head_sha": release_sha,
                  "status": "completed", "conclusion": "success"}],
                {101: jobs_for({n: conclusion if n == "backend" else "success"
                               for n in REQUIRED_JOBS})}, expect_pass=False)

        run("cannot-combine-green-jobs-from-different-runs",
            [{"id": i, "head_sha": release_sha, "status": "completed", "conclusion": "success"}
             for i in (101, 102)],
            {101: jobs_for({n: "success" for n in REQUIRED_JOBS if n != "backend"}),
             102: jobs_for({"backend": "success"})}, expect_pass=False)

        # GitHub's latest-jobs view of a successful failed-jobs rerun retains
        # successful jobs from the earlier attempt. All belong to ONE run.
        rerun_jobs = all_green()
        for job in rerun_jobs:
            job["run_attempt"] = 2 if job["name"] == "backend" else 1
        run("successful-partial-rerun-retains-earlier-green-jobs",
            [{"id": 101, "head_sha": release_sha, "status": "completed",
              "conclusion": "success", "run_attempt": 2}],
            {101: rerun_jobs}, expect_pass=True)

        run("other-commit-green-cannot-satisfy",
            [{"id": 101, "head_sha": ancestor_sha, "status": "completed", "conclusion": "success"}],
            {101: all_green()}, expect_pass=False)

        run("failed-required-job-blocks",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "failure"}],
            {101: jobs_for({**{n: ("success" if n != "backend" else "failure")
                               for n in REQUIRED_JOBS}})},
            expect_pass=False)

        run("pending-required-job-blocks",
            [{"id": 101, "head_sha": release_sha, "status": "in_progress", "conclusion": None}],
            {101: jobs_for({**{n: ("success" if n != "e2e" else None) for n in REQUIRED_JOBS}})},
            expect_pass=False)

        run("skipped-required-job-blocks",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "failure"}],
            {101: jobs_for({**{n: ("success" if n != "docker" else "skipped")
                               for n in REQUIRED_JOBS}})},
            expect_pass=False)

        run("missing-required-job-blocks",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "success"}],
            {101: jobs_for({n: "success" for n in REQUIRED_JOBS if n != "actionlint"})},
            expect_pass=False)

        run("api-failure-fails-closed",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "success"}],
            {101: all_green()}, expect_pass=False, curl_fail="1")

        # A failed run at the SHA plus a green re-run/second run satisfies the
        # gate (re-run semantics): the checks DID pass for that exact commit.
        run("rerun-green-satisfies",
            [{"id": 101, "head_sha": release_sha, "status": "completed", "conclusion": "failure"},
             {"id": 102, "head_sha": release_sha, "status": "completed", "conclusion": "success"}],
            {101: jobs_for({**{n: ("success" if n != "backend" else "failure")
                               for n in REQUIRED_JOBS}}),
             102: all_green()},
            expect_pass=True)

        # Issue #56 hardening: the gate verifies the SHA pinned in prepare, not
        # whatever the mutable tag points at now. Both rejections happen BEFORE
        # any CI API query (asserted via api_calls == 0).
        log, _ = run("pinned-sha-mismatch-blocks",
                     [{"id": 101, "head_sha": release_sha,
                       "status": "completed", "conclusion": "success"}],
                     {101: all_green()}, expect_pass=False, expected_sha=ancestor_sha)
        if log:
            raise SystemExit("pin mismatch must be rejected before any CI API query")

        log, _ = run("empty-pin-fails-closed",
                     [{"id": 101, "head_sha": release_sha,
                       "status": "completed", "conclusion": "success"}],
                     {101: all_green()}, expect_pass=False, expected_sha="")
        if log:
            raise SystemExit("empty pin must be rejected before any CI API query")

        # Tag moved after the pin: HEAD and the tag agree with each other on the
        # MOVED commit (this job even bound to it), but that is not the commit
        # prepare pinned — the gate fails closed. Mutates the fixture repo, so
        # this case runs last.
        git(repo, "checkout", "-q", ancestor_sha)
        git(repo, "tag", "-f", "v9.9.9", ancestor_sha)
        log, _ = run("tag-moved-after-pin-blocks",
                     [{"id": 101, "head_sha": ancestor_sha,
                       "status": "completed", "conclusion": "success"}],
                     {101: all_green()}, expect_pass=False, expected_sha=release_sha)
        if log:
            raise SystemExit("a moved tag must be rejected before any CI API query")

        # ── bind-release-ref: pin enforcement BEFORE checkout ───────────────
        bind_script = extract_bind_script(
            root / ".github/actions/bind-release-ref/action.yml")
        bind_repo = temporary / "bindrepo"
        subprocess.run(["git", "init", "-q", "-b", "main", str(bind_repo)], check=True)
        git(bind_repo, "config", "user.email", "gate-test@example.invalid")
        git(bind_repo, "config", "user.name", "gate-test")
        git(bind_repo, "commit", "--allow-empty", "-q", "-m", "one")
        pinned_sha = git(bind_repo, "rev-parse", "HEAD")
        git(bind_repo, "commit", "--allow-empty", "-q", "-m", "two")
        moved_sha = git(bind_repo, "rev-parse", "HEAD")
        # The dispatched ref (main tip) is a THIRD commit, so a pre-checkout
        # failure is observable: HEAD must stay put instead of moving to the
        # moved tag target.
        git(bind_repo, "commit", "--allow-empty", "-q", "-m", "three")
        git(bind_repo, "tag", "v9.9.9", pinned_sha)

        bind = temporary / "bind.sh"
        bind.write_text(bind_script + "\n")

        def run_bind(name, expect_pass, expect_head=None, publish="1",
                     tag="v9.9.9", expected_sha=None):
            before = git(bind_repo, "rev-parse", "HEAD")
            result = subprocess.run(
                ["bash", str(bind)], cwd=bind_repo, text=True, capture_output=True, timeout=30,
                env={**os.environ, "PUBLISH": publish, "TAG_NAME": tag,
                     "EXPECTED_SHA": pinned_sha if expected_sha is None else expected_sha,
                     "VERSION_REGEX": r"^v?[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$"},
            )
            passed = result.returncode == 0
            if passed != expect_pass:
                raise SystemExit(
                    f"bind-{name}: expected {'pass' if expect_pass else 'fail'}, "
                    f"got exit={result.returncode}\n{result.stdout}\n{result.stderr}"
                )
            after = git(bind_repo, "rev-parse", "HEAD")
            want = before if expect_head is None else expect_head
            if after != want:
                raise SystemExit(f"bind-{name}: HEAD {after}, expected {want}")
            print(f"PASS bind-{name} exit={result.returncode}")
            return result

        # Matching pin: binds HEAD to the pinned tag commit.
        run_bind("matching-pin-binds-tag", expect_pass=True, expect_head=pinned_sha)

        # Tag moved after the pin: fail closed BEFORE the checkout — HEAD must
        # not move at all, so this job can never build a different commit than
        # the one CI verified.
        git(bind_repo, "checkout", "-q", "main")
        git(bind_repo, "tag", "-f", "v9.9.9", moved_sha)
        result = run_bind("tag-moved-after-pin-blocks", expect_pass=False)
        if "refusing to bind" not in result.stderr:
            raise SystemExit("bind-tag-moved-after-pin-blocks: expected the pin mismatch error")

        # A publish bind without the pin fails closed.
        run_bind("missing-pin-fails-closed", expect_pass=False, expected_sha="")

        # Non-publish dry-runs are unchanged: a no-op even with a mismatched pin.
        run_bind("dry-run-untouched-by-pin", expect_pass=True, publish="0",
                 expected_sha=pinned_sha)

        # Missing tag and bad tag names still fail closed.
        run_bind("missing-tag-fails", expect_pass=False, tag="v8.8.8")
        run_bind("bad-semver-fails", expect_pass=False, tag="not-a-version")


if __name__ == "__main__":
    main()
