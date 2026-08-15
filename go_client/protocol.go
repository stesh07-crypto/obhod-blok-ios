package main

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// RequestConfig запрашивает WireGuard конфиг через DTLS-соединение.
func RequestConfig(conn net.Conn, localPort, deviceID, password string) (string, error) {
	payload := fmt.Sprintf("GETCONF:%s|%s|%s", localPort, deviceID, password)
	
	b := make([]byte, 4096)
	var n int
	var err error
	
	// DTLS/UDP не гарантирует доставку Application Data, поэтому повторяем запрос
	for attempt := 1; attempt <= 10; attempt++ {
		if _, writeErr := conn.Write([]byte(payload)); writeErr != nil {
			return "", fmt.Errorf("отправка GETCONF: %w", writeErr)
		}

		if readDeadlineErr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); readDeadlineErr != nil {
			return "", fmt.Errorf("установка дедлайна: %w", readDeadlineErr)
		}
		
		n, err = conn.Read(b)
		if err == nil {
			break // Успешно прочитали
		}
		
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			continue // Тайм-аут, повторяем
		}
		
		// Другая ошибка
		_ = conn.SetReadDeadline(time.Time{})
		return "", fmt.Errorf("чтение ответа конфига: %w", err)
	}
	
	_ = conn.SetReadDeadline(time.Time{})
	
	if err != nil {
		return "", fmt.Errorf("превышено количество попыток запроса конфига: %w", err)
	}

	resp := string(b[:n])
	if resp == "NOCONF" {
		return "", nil
	}

	if strings.HasPrefix(resp, "DENIED:") {
		reason := strings.TrimPrefix(resp, "DENIED:")
		switch reason {
		case "wrong_password":
			return "", fmt.Errorf("FATAL_AUTH: неверный пароль подключения")
		case "expired":
			return "", fmt.Errorf("FATAL_AUTH: срок действия пароля истёк")
		case "device_mismatch":
			return "", fmt.Errorf("FATAL_AUTH: пароль привязан к другому устройству")
		default:
			return "", fmt.Errorf("FATAL_AUTH: доступ запрещён (%s)", reason)
		}
	}

	return resp, nil
}

// SendAuth отправляет команду авторизации, чтобы сервер мог связать соединение с устройством
func SendAuth(conn net.Conn, deviceID, password string) error {
	payload := fmt.Sprintf("AUTH:%s|%s", deviceID, password)
	
	// Отправляем несколько раз для надёжности, так как DTLS Application Data может теряться
	for i := 0; i < 3; i++ {
		if _, err := conn.Write([]byte(payload)); err != nil {
			if i == 0 {
				return fmt.Errorf("отправка AUTH: %w", err)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	return nil
}


