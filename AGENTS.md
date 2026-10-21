# AGENTS.md

## 目的

このファイルは、この repository で作業する coding agent に対する恒久的な project instruction である。

Coding-agent session や会話履歴を durable state の正本にしない。
作業の意味論的な scope、判断、完了条件は GitHub Issue、review / revision instruction、その他の明示された task contract に置く。

## Executable Task Contract

coding agent に依頼する implementation、review / re-review、revision は、いずれも **Executable Task Contract** として扱う。

Executable Task Contract は、その task だけを根拠に、coding agent が新しい Human の product / architecture / security 判断を要求せず、次の reviewable checkpoint まで進めるために十分な情報を持たなければならない。

task contract では、必要に応じて少なくとも次を明確にする。

- 現在の task-relevant state
- target または review objective
- authoritative input / evidence
- in-scope と non-goal / out-of-scope
- acceptance / finding criteria
- 必要な validation / evidence
- required output
- 推測せず STOP すべき条件

task contract の外にある新しい product / architecture / security 判断が必要になった場合は、推測で補完せず Human / coordinator へ返す。

historical Issue、過去会話、abandoned implementation、stale branch などの non-authoritative material から不足した判断を勝手に復元しない。

## Executable Issue

implementation work は原則として GitHub Issue を durable task contract とする。

Executable Issue は、Issue 本文だけを根拠に次の reviewable checkpoint まで実装を進められる work item とする。

基本構成の例:

- Current state
- Target state
- Non-goals
- Acceptance Criteria

見出し名そのものは normative ではない。重要なのは task が executable であること。

別 artifact が authority の場合は bounded reference として明示する。
coding agent が unrelated historical Issue や project history を open-ended に探索しなければ task を理解できない状態にしない。

## Review / re-review

review task も Executable Task Contract の原則に従う。

必要に応じて次を明示する。

- Review target
- Authority / evidence
- Review scope
- Out of scope
- Acceptance / finding criteria
- Required output

review は原則 read-only とする。
task contract が明示的に mutation を許可していない限り、review 中に source、Issue、PR、branch、remote state を変更しない。

review finding は、それ自体で implementation scope を拡張する authorization ではない。

## Revision

review finding を受けた revision も Executable Task Contract の原則に従う。

必要に応じて次を明示する。

- Current finding / failed condition
- Required correction
- Applicable authority
- Constraints / non-goals
- Acceptance Criteria
- Required validation / evidence

authorized finding の修正に必要な範囲だけを変更する。
revision を理由に unrelated redesign や追加実装を行わない。

finding を解消するために新しい Human-owned decision が必要なら STOP して返す。

## GitHub-native review handoff

Pull Request を対象とする implementation / review / revision cycle では、
GitHub 上の PR state を persistent な review / remediation handoff として使用する。

通常の artifact 対応は次とする。

- GitHub Issue: implementation の durable task contract
- Pull Request: review 対象の candidate
- top-level PR conversation comment: review / re-review の Executable Task Contract
- native GitHub Pull Request Review: independent verifier の disposition
- inline review thread: 具体的 finding と remediation discussion
- commit SHA: review / re-review の immutable boundary
- CI / check status: supporting execution evidence

PR review の task contract は、必要に応じて少なくとも次を明示する。

- review type
- authoritative Issue / artifact
- target PR
- exact target HEAD SHA
- review scope と out-of-scope
- acceptance / finding criteria
- required output / disposition

independent reviewer は、bounded な authority と review instruction に基づいて
native GitHub Pull Request Review を提出する。

通常の disposition は `APPROVE` または `REQUEST_CHANGES` とする。

GitHub identity restriction により `APPROVE` / `REQUEST_CHANGES` を
提出できない場合に限り、native GitHub Pull Request Review の
`COMMENT` / `COMMENTED` を verifier disposition の transport fallback として
使用してよい。

この fallback review は少なくとも次を明示する。

- exact reviewed HEAD SHA
- substantive verdict: `PASS` または `RETURN`
- blocking / non-blocking findings
- evidence boundary
- landing recommendation
- native `APPROVE` / `REQUEST_CHANGES` を使用できなかった理由

`COMMENTED` fallback は native GitHub approval ではない。
`COMMENTED + PASS` を `APPROVED` と表現せず、
`COMMENTED + RETURN` を `REQUEST_CHANGES` と表現しない。

fallback は implementation と independent verification の role separation を
緩和しない。同じ GitHub account identity を使用していても、
implementation role が independent verification role を兼ねてよいことにはならない。

repository / branch protection が native approval を要求する場合、
この fallback はその requirement を満たさず、迂回にも使用しない。

review disposition は、実際に review した commit SHA に bind される。
review 後に PR HEAD が変わった場合、以前の review はその reviewed SHA に対する
evidence として残るが、新しい HEAD の landing disposition には自動的にならない。

implementation agent は自己承認しない。
implementation と independent verification の role は分離する。

`REQUEST_CHANGES` を受けた revision では、implementation agent は GitHub 上の
submitted review と relevant review thread を読み、authorized finding だけを修正する。
必要に応じて relevant thread に remediation と resulting commit SHA を返信し、
top-level PR conversation comment に少なくとも次を記録する。

- new HEAD SHA
- addressed findings
- validation / tests
- remaining blocker / STOP state

revision task は、必要な finding / authority が GitHub 上に表現可能である限り、
未提示の chat transcript や未参照の local review file を探索しなければ実行できない
状態にしない。

外部 artifact が本当に必要な場合は、open-ended に探索させず、
revision task contract から explicit かつ bounded に参照する。

differential re-review では、top-level PR conversation comment の task contract に
必要に応じて次を明示する。

- previous reviewed SHA
- current target HEAD SHA
- prior findings to close
- differential review scope
- acceptance / finding criteria
- required disposition

independent reviewer はその bounded delta を review し、current target SHA に bind された
新しい native Pull Request Review を提出する。

review cycle の state は、可能な限り GitHub 上の durable facts から導出する。

- current PR HEAD SHA
- applicable independent review と reviewed SHA
- unresolved review threads
- latest implementation-completion comment
- CI / check status

labels は UI 補助として使用してよいが、authoritative workflow state にしない。

native `APPROVE` は exact reviewed SHA に対する verifier disposition であり、
それ自体は merge authorization ではない。
landing は Human / coordinator の別判断とし、landing 前に candidate HEAD が
accepted review の reviewed SHA と一致することを確認する。
別 policy が明示的に許可しない限り、HEAD drift がある状態で以前の approval を
新しい HEAD に転用しない。

## Workspace isolation

source、configuration、documentation を変更する task は、作業開始前に isolated Git worktree を使用する。

- unrelated または dirty な user checkout 上で直接 implementation / revision を行わない。
- task 用 worktree が存在しない場合は、task が要求する base revision から dedicated branch / worktree を作成する。
- unrelated branch / worktree / file を reset、clean、stash、delete、overwrite しない。
- task ごとに変更を分離し、unrelated change を混ぜない。
- 安全に isolated worktree を確立できない場合は、既存状態を破壊して回避せず STOP して報告する。

Git commit、push、PR 作成、tracker mutation、merge、remote-service mutation は、task contract または coordinator / Human の明示的な authorization に従う。
明示的な authorization が無い destructive / remote mutation を推測で実行しない。

## Scope discipline

- task contract の scope と acceptance criteria を最初に確認する。
- unrelated changes や scope 外の追加実装を行わない。
- architecture / security invariant を実装都合で変更しない。
- implementation 不能な contradiction は推測で解消せず STOP して報告する。
- failure を隠す retry、fallback、権限拡張、global cleanup を勝手に追加しない。
- 「将来便利そう」という理由だけで機能を追加しない。

## Dependency policy

- Go standard library と task contract で明示的に許可された dependency を優先する。
- task contract に無い third-party Go module を勝手に追加しない。
- 新規 third-party dependency が必要なら追加せず STOP し、必要 capability、stdlib / 既存 dependency で不足する理由、maintainer / provenance、transitive impact を報告する。
- GitHub star 数を dependency 採用根拠にしない。
- 小さく監査可能な utility は obscure convenience package より narrow local implementation を優先する。

## Verification

- task contract の acceptance criteria に必要な test / format / vet / build / static check を実行する。
- 実測した事実と推論を区別する。
- 実行していない検証を PASS と扱わない。
- outcome unknown / partial failure を success に丸めない。
- static linking、single-binary portability、CGO、shared-library dependency、Podman / network / cgroup / systemd 等の環境依存特性を未検証で断定しない。
- fake / emulated environment の結果を、task が要求する real-runtime evidence と同一視しない。
- target host、dirty integration、clean deployment、external E2E 等の検証は、その task contract が明示的に所有する場合だけ実行する。

## 報告

Human 向けの作業報告は日本語で行う。

最終報告は簡潔に、少なくとも次を含める。

- 実施した material changes
- 実際に行った verification と結果
- acceptance / finding criteria に影響する未解決事項
- STOP した場合の具体的 blocker
- Human / coordinator による次の操作が必要な場合はその内容

コード識別子、CLI、config key、branch 名、commit message 等の machine-facing identifier は repository の既存 conventions に従う。
