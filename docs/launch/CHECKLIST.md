# Launch checklist

Every step here is performed **by you** (the maintainer) — nothing in this
repo posts, publishes, or pushes anything on its own. Work top to bottom;
don't start the "launch day" section until every "before" box is ticked.

Companion drafts: [HACKER_NEWS.md](HACKER_NEWS.md) ·
[PRODUCT_HUNT.md](PRODUCT_HUNT.md) · shot list in [../DEMO.md](../DEMO.md).

---

## 1. Before the repo goes public

### Legal / hygiene (blocking)

- [ ] **Pick and commit a LICENSE.** There is none in the repo and the README
      badge says "TBD" — HN will ask within minutes and "TBD" reads as
      "not actually open source." (MIT/Apache-2.0 are the boring, safe picks;
      update the README badge in the same commit. `build/release.sh` already
      copies `LICENSE` into archives once it exists.)
- [ ] **Secret sweep, working tree:** confirm no `operator.token`,
      `issuer.key`, `cerberus.db`, `daemon.json`, or personal paths are
      tracked (`git ls-files | grep -Ei "token|\.key|\.db"`).
- [ ] **Secret sweep, history:** run a scanner (e.g. `gitleaks detect`) over
      the full history. If anything real ever landed, rewrite history *before*
      the repo is ever public — you can't unpublish.
- [ ] **Stray-file sweep:** delete or gitignore local debris that must not
      ship (demo `.mp4`s in the repo root, `.cerberus-peripheral-*` scratch
      dirs, `.gocache-test/`, editor configs). `git status` should be clean.
- [ ] **Internal-doc pass:** skim `HANDOFF.md`, `CHAT-HANDOFF.md`,
      `LAUNCH-PLAN.md`, `docs/research/` and decide each stays public as-is.
      (They're honest engineering logs — that's on-brand — just make sure
      nothing names people/machines you don't want named.)

### Product verification (blocking — every public claim gets re-proven)

- [ ] `task build && task test` green on your machine; CI green on
      `integration` (Linux/macOS/Windows matrix, race + fuzz + SBOM jobs).
- [ ] `go run ./test/e2e` prints the 1337 acceptance line.
- [ ] `go run ./test/pipeline_e2e` and `go run ./test/peripheral_e2e` pass.
- [ ] **Walk QUICKSTART.md literally on two real Windows machines** — fresh
      installs, copy-paste each command, including: `nodes` discovery,
      `run --on`, `gpu --on`, `pipeline-run`, `fs put`/`fs get` across nodes,
      `audio play --on`. Fix any doc that doesn't match reality; reality wins.
- [ ] Walk the Tailscale variant once (two networks, `--peer` bootstrap).
- [ ] MCP check: build `cerberus-mcp`, connect Claude Code or Cursor, run the
      "list nodes then run workload" flow from docs/mcp.md.
- [ ] Gateway check: `GET /v1/models` and one `POST /v1/chat/completions`
      with the Bearer token.

### Release artifacts

- [ ] Tag the release (`git tag v0.1.x && git push --tags`) and let
      `release.yml` build; then **edit the draft GitHub Release**: paste
      release notes (what works / what's partial / what's stubbed — reuse the
      README matrix), and publish the draft.
- [ ] Verify assets attached: the Windows `.msi`, per-OS
      `cerberus_<ver>_<os>_<arch>.{zip,tar.gz}` archives, `checksums.txt`.
- [ ] **Download on a clean machine** (not your dev box): SmartScreen flow
      matches the docs, daemon starts, `cerberus status` works. Verify one
      checksum by hand.
- [ ] Confirm the asset filenames match what README.md, QUICKSTART.md,
      TESTERS.md, and the website link to (e.g.
      `Cerberus_0.1.0_x64_en-US.msi`, `cerberus_0.1.0_windows_amd64.zip`).

### Repo presentation

- [ ] Repo description + topics set (`distributed-systems`, `p2p`,
      `capability-security`, `webassembly`, `mesh`, `golang`, `rust`).
- [ ] Social preview image uploaded (Settings → General → Social preview).
- [ ] Issues enabled; a `beta-feedback` label exists; issue template asks for
      `cerberus doctor --json` output.
- [ ] README renders correctly on GitHub (tables, ASCII diagram, all relative
      links resolve — click every link in the Documentation table).
- [ ] Decide the canonical branch story: merge `integration` → `main` so the
      public default branch is the one all docs and the release tag describe.

### Site + video

- [ ] Website deployed and reachable; download buttons point at
      `releases/latest` assets that actually exist; TESTERS/QUICKSTART links
      work from the site.
- [ ] Demo video uploaded (YouTube, unlisted until launch morning). Captions
      follow the honesty rules in docs/DEMO.md (backend lines visible, no
      faked output).
- [ ] Gallery assets for PH exported (5 images/GIFs matching the captions in
      PRODUCT_HUNT.md).

### Accounts / logistics

- [ ] HN account ready (posting Show HN from a zero-karma account is fine,
      but log in and check it's not shadow-banned — ask a friend to view your
      test comment logged-out).
- [ ] PH maker account ready; listing drafted from PRODUCT_HUNT.md; schedule
      set for 12:01 AM Pacific on the chosen day.
- [ ] Pick the sequence and block the calendar. Suggested: **Tue — Product
      Hunt** (you're on PH all day), **Wed or Thu 7–9 AM Pacific — Show HN**
      (you're on HN all morning). Never both the same day; you can't be
      present in two threads.
- [ ] Clear your day(s): no meetings for the first 4 hours after each post.

---

## 2. Launch day — Product Hunt

- [ ] 12:01 AM PT: listing goes live (scheduled). Verify it renders; fix
      images if mangled.
- [ ] Post the maker first-comment (from PRODUCT_HUNT.md) immediately.
- [ ] Morning: share the PH link once on your own channels (personal X/
      LinkedIn/Discords you're actually a member of). No upvote-begging —
      PH and HN both punish it.
- [ ] Reply to **every** comment through the day; concede limitations fast,
      link the code path (the claims→code table in HACKER_NEWS.md works here
      too).
- [ ] Triage new GitHub issues with the `beta-feedback` label as they arrive;
      "filed, thanks — tracking here <link>" is a fine same-day answer.

## 3. Launch day — Show HN

- [ ] 7–9 AM PT, weekday: submit the Show HN (title + body from
      HACKER_NEWS.md). URL = the GitHub repo.
- [ ] Immediately add a top comment if you have anything time-sensitive to
      note (e.g. "unsigned installer warning is expected — beta").
- [ ] First 3 hours at the keyboard. Answer the hard questions with the
      prepared honest answers; for anything new, answer from the code, not
      from memory.
- [ ] If someone finds a real bug or hole: thank them by name in-thread, file
      the issue, link it back. This is the single best possible launch
      content.
- [ ] Don't: argue tone, reply to every flame, edit the post reactively, or
      ask for votes. Do: keep linking to specific files.

## 4. The week after

- [ ] Pin a "known issues from launch week" issue; update TESTERS.md status
      table if any ✅ turned out to be ❌ in the wild.
- [ ] Ship one visible fix fast (the top reproducible beta report) and note it
      in the HN/PH threads — follow-through is remembered.
- [ ] Write down the top 5 recurring questions; fold answers into README/FAQ
      so the docs absorb the load.
- [ ] Thank testers who filed issues (by handle) in the next release notes.
