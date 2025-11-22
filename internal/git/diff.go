package git

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sourcegraph/go-diff/diff"
)

// FileDiff 文件diff信息
type FileDiff struct {
	Filename      string
	Hunks         []HunkDiff
	ChangedLines  []int  // 所有变更的行号
	IsNewFile     bool   // 是否是新文件
	IsDeletedFile bool   // 是否是删除的文件
}

// HunkDiff 代码块diff信息
type HunkDiff struct {
	NewStartLine int32
	NewLines     int32
	AddedLines   []LineDiff
	ModifiedLines []LineDiff // 修改的行
}

// LineDiff 行diff信息
type LineDiff struct {
	LineNumber  int32
	LineContent string
}

// GetGitDiff 获取两个commit之间的diff
func GetGitDiff(repoPath, oldCommit, newCommit string) ([]byte, error) {
	cmd := exec.Command("git", "diff", oldCommit, newCommit)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git diff 失败: %w\n输出: %s", err, string(output))
	}
	return output, nil
}

// ParseDiff 解析diff内容
func ParseDiff(diffContent []byte) ([]FileDiff, error) {
	diffs, err := diff.ParseMultiFileDiff(diffContent)
	if err != nil {
		return nil, err
	}

	var res []FileDiff
	for _, d := range diffs {
		// 如果文件被删除了,则跳过
		if d.NewName == "/dev/null" {
			continue
		}

		// 去掉前缀 a/ 或 b/
		newName := strings.TrimPrefix(d.NewName, "b/")
		oldName := strings.TrimPrefix(d.OrigName, "a/")

		fd := FileDiff{
			Filename:      newName,
			Hunks:         []HunkDiff{},
			ChangedLines:  []int{},
			IsNewFile:     oldName == "/dev/null",
			IsDeletedFile: newName == "/dev/null",
		}

		for _, h := range d.Hunks {
			// 如果新文件的行数为0,则跳过
			if h.NewStartLine == 0 {
				continue
			}

			// 解析 Hunk 的 Body 来获取新增和修改的行
			addedLines := []LineDiff{}
			modifiedLines := []LineDiff{}
			reader := bufio.NewReader(bytes.NewReader(h.Body))
			scanner := bufio.NewScanner(reader)

			currentNewLineNum := h.NewStartLine
			for scanner.Scan() {
				line := scanner.Text()

				// 跳过文件头行
				if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
					continue
				}

				if strings.HasPrefix(line, "+") {
					// 新增行
					addedLines = append(addedLines, LineDiff{
						LineNumber:  currentNewLineNum,
						LineContent: line[1:], // 去掉 '+' 前缀
					})
					fd.ChangedLines = append(fd.ChangedLines, int(currentNewLineNum))
					currentNewLineNum++
				} else if strings.HasPrefix(line, "-") {
					// 删除行: 不影响新文件的行号,但记录为修改
					// 注意: 这里我们主要关注新文件中的变更
					continue
				} else if strings.HasPrefix(line, " ") || line == "" {
					// 上下文行(空格开头)或空行: 在新文件中存在
					currentNewLineNum++
				}
			}

			// 对于修改的行,我们认为是删除后新增的组合
			// 简化处理: 将新增的行视为可能的修改
			modifiedLines = addedLines

			fd.Hunks = append(fd.Hunks, HunkDiff{
				NewStartLine:  h.NewStartLine,
				NewLines:      h.NewLines,
				AddedLines:    addedLines,
				ModifiedLines: modifiedLines,
			})
		}

		res = append(res, fd)
	}

	return res, nil
}

// GetChangedFiles 获取变更的文件列表
func GetChangedFiles(repoPath, oldCommit, newCommit string) ([]string, error) {
	diffContent, err := GetGitDiff(repoPath, oldCommit, newCommit)
	if err != nil {
		return nil, err
	}

	fileDiffs, err := ParseDiff(diffContent)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, fd := range fileDiffs {
		if !fd.IsDeletedFile {
			files = append(files, fd.Filename)
		}
	}

	return files, nil
}
