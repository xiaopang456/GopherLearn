package code

//code响应状态码

type Code int64

const (
	CodeSuccess Code = 1000

	CodeInvalidParams      Code = 2001
	CodeUserExist          Code = 2002
	CodeUserNotExist       Code = 2003
	CodeInvalidPassword    Code = 2004
	CodeNotMatchPassword   Code = 2005
	CodeInvalidToken       Code = 2006
	CodeNotLogin           Code = 2007
	CodeInvalidCaptcha     Code = 2008
	CodeRecordNotFound     Code = 2009
	CodeIllegalPassword    Code = 2010
	CodeCaptchaTooFrequent Code = 2011
	// CodeEmailNotConfigured means the mail path was never configured, so a
	// captcha cannot be sent at all. It is deliberately not CodeServerBusy: the
	// operator has to change the configuration, and waiting does not help.
	CodeEmailNotConfigured Code = 2012
	// CodeEmailSendFailed means the mail path is configured but the relay
	// refused or could not be reached.
	CodeEmailSendFailed Code = 2013

	CodeForbidden Code = 3001

	CodeServerBusy Code = 4001

	AIModelNotFind    Code = 5001
	AIModelCannotOpen Code = 5002
	AIModelFail       Code = 5003

	TTSFail        Code = 6001
	TTSRateLimited Code = 6002

	KnowledgeInvalidFile          Code = 7001
	KnowledgeFileTooLarge         Code = 7002
	KnowledgeEmptyFile            Code = 7003
	KnowledgeDocumentLimit        Code = 7004
	KnowledgeStorageLimit         Code = 7005
	KnowledgeNameConflict         Code = 7006
	KnowledgeContentConflict      Code = 7007
	KnowledgeDocumentNotFound     Code = 7008
	KnowledgeDocumentNotRetryable Code = 7009
	KnowledgeDocumentBusy         Code = 7010

	LearningSessionActive      Code = 8001
	LearningSessionNotFound    Code = 8002
	LearningNoEvidence         Code = 8003
	LearningGenerationFail     Code = 8004
	LearningSessionExpired     Code = 8005
	LearningGradingFail        Code = 8006
	LearningSubmissionConflict Code = 8007
	LearningGradingBusy        Code = 8008
	LearningProgressNotFound   Code = 8009

	MemoryCandidateNotFound Code = 9001
	MemoryNotFound          Code = 9002
	MemoryDreamBusy         Code = 9003
	MemoryConflict          Code = 9004
	MemoryDerivedReadOnly   Code = 9005
	MemoryDraftNotFound     Code = 9006

	AgentRunBusy         Code = 10001
	AgentRequestConflict Code = 10002
	AgentRequestFinished Code = 10003
	SessionLimitReached  Code = 10004
	MessageLimitReached  Code = 10005
)

var msg = map[Code]string{
	CodeSuccess: "success",

	CodeInvalidParams:      "请求参数错误",
	CodeUserExist:          "用户名已存在",
	CodeUserNotExist:       "用户不存在",
	CodeInvalidPassword:    "用户名或密码错误",
	CodeNotMatchPassword:   "两次密码不一致",
	CodeInvalidToken:       "无效的Token",
	CodeNotLogin:           "用户未登录",
	CodeInvalidCaptcha:     "验证码错误",
	CodeRecordNotFound:     "记录不存在",
	CodeIllegalPassword:    "密码不合法",
	CodeCaptchaTooFrequent: "验证码发送过于频繁，请稍后重试",
	CodeEmailNotConfigured: "邮件服务未配置，无法发送验证码：请在配置文件的 [emailConfig] 段填写发件地址与授权码",
	CodeEmailSendFailed:    "验证码邮件发送失败，请稍后重试或检查邮件服务配置",

	CodeForbidden: "权限不足",

	CodeServerBusy: "服务繁忙",

	AIModelNotFind:    "模型不存在",
	AIModelCannotOpen: "无法打开模型",
	AIModelFail:       "模型运行失败",
	TTSFail:           "语音服务失败",
	TTSRateLimited:    "语音请求过于频繁，请稍后重试",

	KnowledgeInvalidFile:          "仅支持有效的 .md 或 .txt 文件",
	KnowledgeFileTooLarge:         "文件超过大小限制",
	KnowledgeEmptyFile:            "文件内容不能为空",
	KnowledgeDocumentLimit:        "文档数量已达到上限",
	KnowledgeStorageLimit:         "知识库总容量已达到上限",
	KnowledgeNameConflict:         "当前知识库已存在同名文档",
	KnowledgeContentConflict:      "当前知识库已存在相同内容",
	KnowledgeDocumentNotFound:     "知识文档不存在",
	KnowledgeDocumentNotRetryable: "只有索引失败的文档可以重试",
	KnowledgeDocumentBusy:         "知识文档正在处理，请稍后重试",

	LearningSessionActive:      "当前会话已有进行中的学习测试",
	LearningSessionNotFound:    "当前没有可恢复的学习测试",
	LearningNoEvidence:         "个人知识库中没有足够的相关证据用于出题",
	LearningGenerationFail:     "学习题目生成失败，请稍后重试",
	LearningSessionExpired:     "学习测试已过期，请重新开始",
	LearningGradingFail:        "答案评分失败，本题未推进，可以重新提交",
	LearningSubmissionConflict: "本次提交编号已被其他答案使用，请刷新后重试",
	LearningGradingBusy:        "该答案正在评分，请勿重复提交",
	LearningProgressNotFound:   "该主题还没有有效的学习进度",

	MemoryCandidateNotFound: "当前没有待整理的记忆候选",
	MemoryNotFound:          "长期记忆不存在",
	MemoryDreamBusy:         "该记忆候选正在整理，请稍后重试",
	MemoryConflict:          "记忆已发生变化，请刷新后重试",
	MemoryDerivedReadOnly:   "学习状态来自真实答题记录，请通过评分纠正功能更新",
	MemoryDraftNotFound:     "待确认记忆草稿不存在",

	AgentRunBusy:         "当前会话已有 Agent 任务正在执行",
	AgentRequestConflict: "该请求编号已被其他内容使用",
	AgentRequestFinished: "该请求已经结束，不能重复执行",
	SessionLimitReached:  "会话数量已达到上限，请删除旧会话后重试",
	MessageLimitReached:  "当前会话消息已达到上限，请新建会话继续",
}

func (code Code) Code() int64 {
	return int64(code)
}

// Msg 获取响应消息
func (code Code) Msg() string {
	if m, ok := msg[code]; ok {
		return m
	}
	return msg[CodeServerBusy]
}
