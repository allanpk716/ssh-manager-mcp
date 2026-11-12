package store

// SecretValueForUpload 是上传内容填值专用的机密项当前值。Value 只允许在
// 进程内使用，不能进入工具结果、日志或审计。
type SecretValueForUpload struct {
	Value             []byte
	WritableServerIDs []string
}

// SecretValuesForUpload 按名字精确解密被引用的机密项。单个值解密失败只
// 返回名字，其他值继续返回；数据库读取失败才返回错误。
func (s *Store) SecretValuesForUpload(names []string) (map[string]SecretValueForUpload, []string, error) {
	if len(names) == 0 {
		return map[string]SecretValueForUpload{}, nil, nil
	}
	requested := make(map[string]bool, len(names))
	for _, name := range names {
		requested[name] = true
	}
	rows, err := s.db.Query(`SELECT id,name,value_blob FROM secrets ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]SecretValueForUpload, len(requested))
	ids := make(map[string]string)
	var failed []string
	for rows.Next() {
		var id, name string
		var encrypted []byte
		if err := rows.Scan(&id, &name, &encrypted); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !requested[name] {
			continue
		}
		ids[id] = name
		value, openErr := open(s.masterKey, encrypted)
		if openErr != nil {
			failed = append(failed, name)
			continue
		}
		values[name] = SecretValueForUpload{Value: value}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}

	allows, err := s.db.Query(`SELECT secret_id,server_id FROM secret_server_allow ORDER BY secret_id,server_id`)
	if err != nil {
		return nil, nil, err
	}
	defer allows.Close()
	for allows.Next() {
		var secretID, serverID string
		if err := allows.Scan(&secretID, &serverID); err != nil {
			return nil, nil, err
		}
		name, exists := ids[secretID]
		if !exists {
			continue
		}
		value := values[name]
		value.WritableServerIDs = append(value.WritableServerIDs, serverID)
		values[name] = value
	}
	if err := allows.Err(); err != nil {
		return nil, nil, err
	}
	return values, failed, nil
}
