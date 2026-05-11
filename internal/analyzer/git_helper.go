package analyzer

import "github.com/jimyag/ripples/internal/git"

// GetGitDiffContent 获取 git diff 内容
func GetGitDiffContent(repoPath, oldCommit, newCommit string) ([]byte, error) {
	return git.GetGitDiff(repoPath, oldCommit, newCommit)
}

// ExtractChangedGoFiles 从 diff 内容中提取变更的 Go 文件列表
func ExtractChangedGoFiles(diffContent []byte) []string {
	fileDiffs, err := git.ParseDiff(diffContent)
	if err != nil {
		return nil
	}

	var changedFiles []string
	for _, fileDiff := range fileDiffs {
		if fileDiff.IsDeletedFile {
			continue
		}

		// 只包含运行时代码文件；测试文件不影响服务部署范围。
		if isRuntimeGoFile(fileDiff.Filename) {
			changedFiles = append(changedFiles, fileDiff.Filename)
		}
	}

	return changedFiles
}
