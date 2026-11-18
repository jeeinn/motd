-- 0002_seed_nodes：P0 版块种子数据（P0-09）
-- 依据：评审稿 v0.2 §6 的 D4（在 /dev/ai 发主题）与 §5.2 的 Node 定义。
--
-- 幂等性：nodes.slug 上有 UNIQUE，这里一律用 INSERT OR IGNORE。
-- 正常路径下 schema_migrations 已保证每个版本只执行一次；即使运维手工重放本文件、
-- 或从旧备份恢复后版本登记丢失，也不会产生重复版块或覆盖用户改过的名称。
INSERT OR IGNORE INTO nodes (slug, name, description) VALUES
  ('/dev/ai',   'AI 与大模型', '模型、工具链、Agent 与工程实践'),
  ('/dev/rust', 'Rust',        '语言特性、生态与项目实践'),
  ('/dev/jobs', '工作与招聘',  '岗位、团队与职业发展'),
  ('/etc/rant', '随便聊聊',    '不设主题的闲谈与吐槽'),
  ('/bin/help', '帮助与支持',  '使用问题、反馈与建议');
