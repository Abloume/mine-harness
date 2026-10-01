---
name: file-ops-policy
description: 文件操作安全规范。执行文件操作（read/write/delete/rename）前必须遵守：删除或覆盖前先备份、备份失败即停止、涉及删除需确认。
tags: [files, safety]
---

# 文件操作安全规范

执行任何文件操作（read / write / delete / rename）前，必须遵守以下规则：

1. 删除或覆盖文件前，必须先调用 `backup` 工具把文件备份到 `backup/` 目录；
2. 执行删除/覆盖前，必须先通过 `read_skill_ref` 读取本技能的引用 `references/safety.md`，按其中的检查清单逐项核对；
3. 备份失败或检查清单未通过则停止操作并汇报，不得强行继续；
4. 涉及 delete 的动作，执行前向用户说明原因（审批由 harness 自动处理时，配合其流程说明）；
5. 汇报结果时说明"已备份 / 未备份"，便于用户核验。

违反以上任一规则视为违规操作。
