package tools

// platformDesktopOps 暂无任何平台的屏幕/输入后端，返回 nil（调用方据此返回 desktop_unsupported）。
// 各平台后端随后续提交加入，并把本文件的构建约束收窄为其余平台。
func platformDesktopOps() *DesktopOps { return nil }
