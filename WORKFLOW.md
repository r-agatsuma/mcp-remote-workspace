# WORKFLOW.md

## Goal

Supplied Issue を、この repository の isolated iro workspace で実装する。

## iro execution model

- managed `iro run` / `iro revise` が渡した Issue/PR context をその execution unit の task specification として扱う。
- `iro run <issue>` の invocation 自体が、その Issue を実装する選択である。
- Parent / Refs / sibling Issue 番号は traceability であり、worker が tracker traversal して本文や comments を補完する前提にしない。
- 実装判断に必要な normative requirement は supplied Issue 自身に存在しなければならない。
- predecessor の結果が必要な場合は、repository state または supplied Issue/comment に明示された input だけを使う。必要 artifact が無ければ STOP して報告する。
- worker は `gh` や tracker fetch を行わない。
- iro が Author 終了後の `git add / commit / push / PR` delivery を所有する。worker は Git を read-only inspection にだけ使う。
- worker は SSH、cloud API、Tunnel、remote systemd、remote Podman 等の remote-service mutation を行わない。
- delivery 後の exact PR HEAD を必要とする OCI/real-runtime verification は、対応する external verification Issue で行う。

## Scope

- supplied Issue の Current state / Target state / Non-goals / Acceptance criteria を満たす変更だけを行う。
- unrelated changes や scope 外の追加実装を行わない。
- architecture/security invariant を実装都合で変更しない。実装不能な contradiction は STOP して報告する。
- failure を隠す retry、fallback、権限拡張、global cleanup を勝手に追加しない。

## Dependency policy

- Go standard library と supplied Issue で明示的に許可された dependency を優先する。
- supplied Issue に無い third-party Go module を勝手に追加しない。
- 新規 third-party dependency が必要なら追加せず STOP し、必要 capability、stdlib/既存 dependency で不足する理由、maintainer/provenance、transitive impact を報告する。
- GitHub star 数を dependency 採用根拠にしない。
- 小さく監査可能な utility は obscure convenience package より narrow local implementation を優先する。

## Verification

- supplied Issue の local acceptance criteria に必要な test / format / vet / build を実行する。
- 実測した事実と推論を区別する。
- 実行していない検証を PASS と扱わない。
- static linking、single-binary portability、CGO、shared-library dependency、Podman/network/cgroup/systemd 等の環境依存特性を未検証で断定しない。
- Apple Container / fake test を real Podman evidence と扱わない。
- outcome unknown / partial failure を success に丸めない。
- target-host / dirty OCI / clean deployment の検証は managed Author の責務ではない。

## Report

最終 Author report は日本語で、以下だけを簡潔にまとめる。

- material changes
- 実際に行った local verification と結果
- acceptance criteria に影響する未解決事項
- STOP した場合の具体的 blocker
