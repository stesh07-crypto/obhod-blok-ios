package main

import "strings"

// workerErrorHint возвращает короткую подсказку для пользователя по тексту ошибки воркера.
func workerErrorHint(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "dtls_timeout"):
		return "таймаут рукопожатия DTLS — сервер или relay не ответили вовремя"
	case strings.Contains(text, "wrap_auth_timeout"):
		return "таймаут рукопожатия WRAP/DTLS — сервер не ответил вовремя"
	case strings.Contains(text, "canceled") || strings.Contains(text, "cancelled"):
		return "соединение прервано до завершения операции"
	case strings.Contains(text, "context deadline exceeded"):
		return "таймаут handshake — сервер не отвечает, проверьте VPS, файрвол и пароль WRAP"
	case strings.Contains(text, "deadline exceeded") || strings.Contains(text, "timeout") || strings.Contains(text, "i/o timeout"):
		return "таймаут — сервер не отвечает, проверьте доступность VPS и пароль"
	case strings.Contains(text, "connection refused"):
		return "сервер отклонил подключение — проверьте IP, порт DTLS и что wdtt-server запущен"
	case strings.Contains(text, "connection reset"):
		return "сервер сбросил соединение — возможен неверный пароль WRAP или перезапуск сервера"
	case strings.Contains(text, "no route") || strings.Contains(text, "network is unreachable"):
		return "нет маршрута до сервера — проверьте интернет; отключите другие VPN/прокси"
	case strings.Contains(text, "lookup") || strings.Contains(text, "no such host"):
		return "DNS не резолвит адрес — смените DNS в ⚙️ → Сеть"
	case strings.Contains(text, "turn квота") || strings.Contains(text, "turn_quota") || strings.Contains(text, "quota") || strings.Contains(text, "486"):
		return "VK исчерпал TURN-слоты — уменьшите потоки или смените VK-хеш/аккаунт"
	case strings.Contains(text, "turn_auth_rejected"):
		return "отказ авторизации TURN — реквизиты отклонены сервером"
	case strings.Contains(text, "turn_forbidden"):
		return "запрос отклонён сервером TURN (403 Forbidden)"
	case strings.Contains(text, "turn_stale_nonce"):
		return "устаревший nonce TURN (438) — требуется повтор запроса с новым nonce"
	case strings.Contains(text, "turn_capacity"):
		return "сервер TURN перегружен (508 Insufficient Capacity)"
	case strings.Contains(text, "turn_attr_not_found") || strings.Contains(text, "attribute not found"):
		return "неполный ответ STUN/TURN сервера — ответ не содержит обязательного атрибута"
	case strings.Contains(text, "turn allocate"):
		return "ошибка TURN relay при попытке выделения адреса"
	case strings.Contains(text, "rate limit") || strings.Contains(text, "flood") || strings.Contains(text, "error 29") || strings.Contains(text, "error_code=9") || strings.Contains(text, "error_code: 9"):
		return "VK временно ограничил запросы (Flood Control) — шлюз ожидает кулдаун перед повтором"
	case strings.Contains(text, "rtp aead") || strings.Contains(text, "auth failed") || strings.Contains(text, "tag mismatch"):
		return "ошибка WRAP/RTP — неверный пароль или несовместимая версия сервера"
	case strings.Contains(text, "fatal_auth") || strings.Contains(text, "неверный пароль"):
		return "неверный пароль подключения или истёк срок действия"
	case strings.Contains(text, "cannot create socket"):
		return "не удалось открыть UDP-сокет — ограничение прошивки или нет сети"
	default:
		return ""
	}
}
