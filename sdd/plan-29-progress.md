Task 1: complete (commits 049aca0..59d475c, review clean)
  Minor(final-review): report overstates add-mode nil-checks; snapshotDraft includes clearcredential key in add mode (unreachable, T2 owns invariant)
Task 2: complete (commits 59d475c..356d8a4, review clean)
  ROOT-CAUSE DISCOVERY (verified app.go:150-163): App.Update drops huh nextFieldMsg/nextGroupMsg for overlays -> embedded forms cannot advance in real terminals (affects formOverlay/importflow/wizard = add flow). serverEditPage works around via internal pump. -> T4 adds backlog item; separate plan candidate
  Minor(final-review): WindowSizeMsg does not re-width open field form (latent); pump couples to huh internals (comment documents); solid cursor cosmetic; weak 22-assert in initial-view test
Task 3: complete (commits 356d8a4..e720660, review clean)
  Minor(final-review): dispatch-table no-op rows wantInitCmd inexpressible-N/A; e-key Init-cmd assertion relaxed (compensated structurally)
Task 4: complete (commits e720660..c38dcd2, review clean)
  Minor(final-review): "上面的独占清除路径" direction ambiguity; stale 编辑表单 wording at managing-servers:179/232 (out of add-scope); backlog#9 fix-direction wording imprecise (messages-vs-cmds; task-specified text — future plan corrects)
