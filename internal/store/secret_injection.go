package store

// SecretEnvironmentValue 是一台服务器执行命令前可能注入的环境变量值。
// 该类型只供进程内部使用，不能序列化到工具结果、日志或审计。
type SecretEnvironmentValue struct {
	EnvironmentName   string
	Value             []byte
	WritableServerIDs []string
}

// SecretEnvironmentReadFailure 记录一个无法解密的注入变量及其可写入服务器。
type SecretEnvironmentReadFailure struct {
	EnvironmentName   string
	WritableServerIDs []string
}

// SecretEnvironmentValuesForInjection 读取全部带环境变量名的机密项当前值。
// 单个值解密失败只返回对应变量名，不阻止其他值继续返回；
// 数据库读取失败才返回错误。错误与失败名单都不包含值。
func (s *Store) SecretEnvironmentValuesForInjection() (values []SecretEnvironmentValue, failures []SecretEnvironmentReadFailure, err error) {
	rows, err := s.db.Query(`
		SELECT id, COALESCE(env_name,''), value_blob
		FROM secrets
		WHERE COALESCE(env_name,'') <> ''
		ORDER BY env_name`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	ids := make(map[string]int)
	failedIDs := make(map[string]int)
	for rows.Next() {
		var id, environmentName string
		var encrypted []byte
		if err := rows.Scan(&id, &environmentName, &encrypted); err != nil {
			return nil, nil, err
		}
		value, openErr := open(s.masterKey, encrypted)
		if openErr != nil {
			failedIDs[id] = len(failures)
			failures = append(failures, SecretEnvironmentReadFailure{EnvironmentName: environmentName})
			continue
		}
		ids[id] = len(values)
		values = append(values, SecretEnvironmentValue{
			EnvironmentName: environmentName,
			Value:           value,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}

	allows, err := s.db.Query(`SELECT secret_id, server_id FROM secret_server_allow ORDER BY secret_id, server_id`)
	if err != nil {
		return nil, nil, err
	}
	defer allows.Close()
	for allows.Next() {
		var secretID, serverID string
		if err := allows.Scan(&secretID, &serverID); err != nil {
			return nil, nil, err
		}
		if index, exists := ids[secretID]; exists {
			values[index].WritableServerIDs = append(values[index].WritableServerIDs, serverID)
		}
		if index, exists := failedIDs[secretID]; exists {
			failures[index].WritableServerIDs = append(failures[index].WritableServerIDs, serverID)
		}
	}
	return values, failures, allows.Err()
}
