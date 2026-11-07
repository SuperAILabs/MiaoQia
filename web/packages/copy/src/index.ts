export const navigation = {
  briefings: "今日简报",
  signals: "信号",
  inbox: "待我处理",
  settings: "设置",
  unavailable: "暂不可用",
} as const;

export const problemCopy: Record<string, { title: string; next: string }> = {
  unauthenticated: { title: "请先登录", next: "登录后才能查看属于你的信号、对象和证据。" },
  forbidden: { title: "没有访问权限", next: "请确认你仍参与这个目标，或联系管理员。" },
  not_found: { title: "内容不存在或不可见", next: "内容可能已撤回、已失效，或当前成员无权查看。" },
  unavailable: { title: "暂时无法读取", next: "服务恢复后可以重试；当前不会用空结果代替未接入数据。" },
  unknown: { title: "读取失败", next: "请稍后重试；如果问题持续，请把操作时间交给支持人员。" },
};

export const statusCopy = {
  not_connected: "未接入",
  unavailable: "暂不可用",
  history_unavailable: "历史视角不可用",
  empty: "暂无记录",
  unknown: "未知",
} as const;
