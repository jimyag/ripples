package output

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jimyag/ripples/internal/analyzer"
)

// Reporter 结果报告器
type Reporter struct {
	results []analyzer.AffectedBinary
}

// NewReporter 创建报告器
func NewReporter(results []analyzer.AffectedBinary) *Reporter {
	return &Reporter{
		results: results,
	}
}

// PrintText 打印文本格式的报告
func (r *Reporter) PrintText() {
	if len(r.results) == 0 {
		fmt.Println("✅ 未检测到受影响的服务。")
		return
	}

	fmt.Printf("🔍 检测到 %d 个受影响的服务:\n", len(r.results))
	fmt.Println(strings.Repeat("-", 50))

	for _, res := range r.results {
		fmt.Printf("📦 Service: \033[1;32m%s\033[0m\n", res.Name) // Green color for service name
		fmt.Printf("   📍 Main Package: %s\n", res.PkgPath)
		fmt.Println("   🔗 Call Chain:")

		for i, node := range res.TracePath {
			prefix := "      "
			if i == 0 {
				prefix = "      🚀 " // Start
			} else if i == len(res.TracePath)-1 {
				prefix = "      🏁 " // End
			} else {
				prefix = "      ⬇️ "
			}

			// Highlight changed symbol
			if strings.Contains(node, "(Changed)") {
				fmt.Printf("%s\033[1;31m%s\033[0m\n", prefix, node) // Red for changed symbol
			} else {
				fmt.Printf("%s%s\n", prefix, node)
			}
		}
		fmt.Println(strings.Repeat("-", 50))
	}
}

// PrintJSON 打印JSON格式的报告
func (r *Reporter) PrintJSON() error {
	jsonData, err := json.MarshalIndent(r.results, "", "  ")
	if err != nil {
		return fmt.Errorf("生成JSON失败: %w", err)
	}

	fmt.Println(string(jsonData))
	return nil
}

// PrintSummary 打印简短摘要
func (r *Reporter) PrintSummary() {
	fmt.Printf("受影响的服务: %d 个\n", len(r.results))
	for _, res := range r.results {
		fmt.Printf("- %s\n", res.Name)
	}
}

// PrintSimple 打印简化格式 - 仅服务名，每行一个（适合脚本解析）
func (r *Reporter) PrintSimple() {
	for _, res := range r.results {
		fmt.Println(res.Name)
	}
}
