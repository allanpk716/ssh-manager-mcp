package softwareinventory

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"time"
)

// csvHeader 是平铺 CSV 总表的固定列顺序。
var csvHeader = []string{"设备", "软件名", "版本", "类别", "监听端口", "暴露级别", "采集时间"}

// MarshalCSV 把统一查询结果序列化成平铺 CSV 总表。每个软件条目一行；
// 一条软件有多个监听端口时仍是一行，两个单元格按端口顺序列出全部值。
// 输出是带 UTF-8 字节顺序标记的字节，行结束符为回车加换行。
func MarshalCSV(result QueryResult) ([]byte, error) {
	var buffer bytes.Buffer
	// 字节顺序标记帮助表格软件把文件识别为 UTF-8。
	buffer.WriteString("\uFEFF")
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write(csvHeader); err != nil {
		return nil, err
	}
	for _, device := range result.Devices {
		for _, item := range device.Items {
			record := []string{
				device.DeviceName, item.Name, item.Version, item.CategoryLabel,
				csvListeningPorts(item.ListeningPorts),
				csvExposureLevels(item.ListeningPorts),
				csvCollectedAt(item.CollectedAt),
			}
			if err := writer.Write(record); err != nil {
				return nil, err
			}
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, fmt.Errorf("生成软件清单 CSV 失败: %w", err)
	}
	return buffer.Bytes(), nil
}

func csvListeningPorts(ports []ListeningPort) string {
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, fmt.Sprintf("%s:%d/%s", port.Address, port.Port, port.Protocol))
	}
	return strings.Join(values, ";")
}

func csvExposureLevels(ports []ListeningPort) string {
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, port.Exposure.Label())
	}
	return strings.Join(values, ";")
}

func csvCollectedAt(collectedAt *time.Time) string {
	if collectedAt == nil {
		return ""
	}
	return collectedAt.UTC().Format(time.RFC3339)
}
