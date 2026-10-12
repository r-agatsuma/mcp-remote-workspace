# WORKFLOW.md

## Goal

Issue に記述された作業を、この repository の isolated workspace で実施する。

## Authority and scope

- 作業開始時に、対象 Issue の目的、acceptance criteria、Parent Design Authority、Parent Product Contract、dependencies を確認する。
- executable child は、その exact Issue を指定した Human execution grant が存在する場合だけ実行する。
- Human grant の execution mode / mutation scope を超えない。
- sibling Issue、downstream Issue、過去の grant、Design Authority の acceptance から実行権限を継承しない。
- Issue / launcher / Human grant / #20 Design Authority の内容が衝突する場合は、実装で補完せず STOP して Human に返す。
- unrelated changes や scope 外の追加実装を行わない。
- architecture/security invariant を実装都合で変更しない。必要なら Design Authority change-control に戻す。

## Dependency policy

- Go standard library と、Issue/contract で承認済みの dependency を優先する。
- 未承認の third-party Go module を勝手に追加しない。
- 新規 third-party dependency が必要な場合は、追加して続行せず STOP し、必要な capability、stdlib/既存 dependency で不足する理由、maintainer/provenance、transitive dependency impact を報告する。
- GitHub star 数や「便利そう」で dependency を採用しない。
- 小さく監査可能な utility のために obscure convenience package を追加するより、scope 内で narrow local implementation を優先する。

## Verification

- Issue の acceptance criteria に必要な test を実行する。
- 実測した事実と、コード/仕様からの推論を区別して報告する。
- 実行していない検証を PASS と扱わない。
- static linking、single-binary portability、CGO の有無、shared-library dependency、Podman/network/cgroup/systemd の挙動など、環境依存の性質を検証なしで断定しない。
- Apple Container 上の unit/fake test を real Podman evidence として扱わない。
- dirty OCI A1 での real-runtime 検証は、その exact child の Human grant が許可する場合だけ実行する。
- outcome unknown / partial failure を success に丸めず、既知/未知を明示する。

## Worker rules

- isolated workspace / git-worktree の対象 Issue だけを変更する。
- 必要な test、format、vet 等を scope 内で実行する。
- remote mutation、host mutation、cleanup、deployment は grant の範囲に限定する。
- failure を隠すための retry、fallback、権限拡張、global cleanup を勝手に追加しない。
- 作業結果、実行した verification、未検証事項、残存 blocker を日本語で要約する。
