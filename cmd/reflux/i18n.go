package main

import "fmt"

// lang is a language of the bot. The CLI speaks English.
type lang string

const (
	langEN lang = "en"
	langRU lang = "ru"
)

// phrase is a message used as an argument of another one, translated
// along with it: "node %s: %s" with the access state as a phrase.
type phrase struct {
	id   string
	args []any
}

func ph(id string, args ...any) phrase { return phrase{id, args} }

// tr renders message id in l, English when l has no text for it.
func tr(l lang, id string, args ...any) string {
	m, ok := messages[id]
	if !ok {
		return id
	}
	format := m[0]
	if l == langRU && m[1] != "" {
		format = m[1]
	}
	out := make([]any, len(args))
	for i, a := range args {
		if p, ok := a.(phrase); ok {
			a = tr(l, p.id, p.args...)
		}
		out[i] = a
	}
	return fmt.Sprintf(format, out...)
}

// messages are the texts in English and Russian. Both take the same
// arguments in the same order.
var messages = map[string][2]string{
	// doctor
	"module.ok":             {"amneziawg kernel module loaded", "модуль ядра amneziawg загружен"},
	"module.fail":           {"amneziawg kernel module not loaded: sudo modprobe amneziawg (after a kernel update: sudo dkms autoinstall)", "модуль ядра amneziawg не загружен: sudo modprobe amneziawg (после обновления ядра: sudo dkms autoinstall)"},
	"hostrule.ok":           {"host routing: egress tunnels bypass the host's own VPN", "маршрутизация: туннели egress идут мимо VPN сервера"},
	"hostrule.fail":         {"%s", "маршрутизация: %s"},
	"configs.ok":            {"egress configs: world %s; russia %s", "конфиги egress: мир %s; Россия %s"},
	"configs.noworld":       {"no world-*.conf in %s: the egress has no way out", "нет world-*.conf в %s: у egress нет выхода"},
	"configs.noru":          {"no ru-*.conf in %s, and no ru-direct file", "нет ru-*.conf в %s и нет файла ru-direct"},
	"perm.warn":             {"%s is readable by others (%v): chmod 600 %s", "%s доступен другим пользователям (%v): chmod 600 %s"},
	"docker.ok":             {"docker %s, compose %s", "docker %s, compose %s"},
	"docker.down":           {"docker does not answer: is it running, and is this user in the docker group?", "docker не отвечает: запущен ли он, есть ли пользователь в группе docker?"},
	"compose.missing":       {"docker compose plugin missing: sudo apt install docker-compose-plugin", "нет плагина docker compose: sudo apt install docker-compose-plugin"},
	"image.ok":              {"image %s: commit %s, built %s", "образ %s: коммит %s, собран %s"},
	"image.local":           {"image %s (local build)", "образ %s (локальная сборка)"},
	"image.missing":         {"image %s not pulled: reflux update", "образ %s не скачан: reflux update"},
	"egress.ok":             {"egress container %s", "контейнер egress: %s"},
	"egress.down":           {"egress container %s: reflux apply", "контейнер egress: %s — reflux apply"},
	"egress.nostatus":       {"egress container %s, but has no status yet (starting?): reflux logs egress", "контейнер egress: %s, но статуса ещё нет (запускается?): reflux logs egress"},
	"egress.error":          {"egress reports %s", "egress сообщает: %s"},
	"world.up":              {"world up via %s (since %s)", "мир: работает через %s (с %s)"},
	"world.down":            {"world DOWN via %s: reflux logs egress", "мир: НЕ работает (%s): reflux logs egress"},
	"russia.up":             {"russia up %s, %d prefixes (list from %s)", "Россия: работает %s, %d сетей (список от %s)"},
	"russia.down":           {"russia DOWN %s: reflux logs egress", "Россия: НЕ работает (%s): reflux logs egress"},
	"ru.direct":             {"direct", "напрямую"},
	"ru.tunnel":             {"via tunnel", "через туннель"},
	"ru.tunnelconf":         {"tunnel %s", "туннель %s"},
	"ru.tunnelfallback":     {"tunnel %s, direct while it does not answer", "туннель %s, напрямую, пока он не отвечает"},
	"ru.fallback":           {"direct (fallback)", "напрямую (резерв)"},
	"russia.fallback":       {"russia leaves directly as a fallback: its tunnel does not answer; it goes back after 5 minutes of answers", "Россия идёт напрямую как резерв: туннель не отвечает; вернётся в туннель после 5 минут стабильной работы"},
	"killswitch":            {"kill switch stopped %d packets", "kill switch остановил пакетов: %d"},
	"clients.err":           {"clients: %v", "клиенты: %v"},
	"clients.none":          {"no clients yet: reflux add <name> --url <document-url>", "клиентов пока нет: reflux add <имя> --url <ссылка>"},
	"node.ok":               {"node %s: up %s, client %s, %s down / %s up", "нода %s: работает %s, клиент %s, ↓%s ↑%s"},
	"node.down":             {"node %s not running: reflux apply", "нода %s не запущена: reflux apply"},
	"node.stranded":         {"node %s started before egress and has no network: reflux heal", "нода %s запущена раньше egress и осталась без сети: reflux heal"},
	"node.doc":              {"node %s lost its document %d times in 5 minutes: reflux logs %s", "нода %s теряла документ %d раз за 5 минут: reflux logs %s"},
	"node.nostatus":         {"node %s gives no status (starting, or set up by an older reflux: reflux update)", "нода %s не отдаёт статус (запускается или создана старой версией reflux: reflux update)"},
	"node.inactive":         {"node %s: %s, not running", "нода %s: %s, не запущена"},
	"node.inactive.running": {"node %s runs although access is %s: reflux heal", "нода %s работает, хотя доступ %s: reflux heal"},
	"cron.ok":               {"cron runs reflux heal", "cron запускает reflux heal"},
	"cron.missing":          {"cron does not run reflux heal: add with crontab -e:\n      * * * * * %s heal >> %s 2>&1", "cron не запускает reflux heal: добавьте через crontab -e:\n      * * * * * %s heal >> %s 2>&1"},
	"disk.ok":               {"disk %s %d%% used", "диск %s занят на %d%%"},
	"disk.warn":             {"disk %s %d%% full", "диск %s заполнен на %d%%"},
	"disk.full":             {"disk %s %d%% full: free space (docker image prune)", "диск %s заполнен на %d%%: освободите место (docker image prune)"},
	"disk.dffail":           {"disk: df failed: %v", "диск: df не сработал: %v"},

	// durations
	"dur.s": {"%ds", "%d с"},
	"dur.m": {"%dm", "%d мин"},
	"dur.h": {"%dh", "%d ч"},
	"dur.d": {"%dd", "%d дн"},

	// states
	"online":         {"online", "в сети"},
	"offline":        {"offline", "не в сети"},
	"access.active":  {"active", "бессрочно"},
	"access.paused":  {"paused", "на паузе"},
	"access.expired": {"expired", "истёк"},
	"access.until":   {"until %s", "до %s"},

	// bot
	"ui.started":        {"🛰 ReFlux bot started on %s: %d problem(s), %d warning(s).", "🛰 Бот ReFlux запущен на %s: проблем %d, предупреждений %d."},
	"ui.egress.none":    {"❌ egress gives no status (not running?)", "❌ egress не отдаёт статус (не запущен?)"},
	"ui.world":          {"🌍 World: %s", "🌍 Мир: %s"},
	"ui.russia":         {"🇷🇺 Russia: %s", "🇷🇺 Россия: %s"},
	"ui.killswitch":     {"🛡 Kill switch: %d blocked", "🛡 Kill switch: заблокировано %d"},
	"ui.clients":        {"👥 <b>Clients</b>", "👥 <b>Клиенты</b>"},
	"ui.clients.none":   {"no clients yet: ➕ Add", "клиентов пока нет: ➕ Добавить"},
	"ui.clients.title":  {"👥 <b>Clients</b>: %d", "👥 <b>Клиенты</b>: %d"},
	"ui.clients.hint":   {"🟢 online · ⚪ offline · ⏸ paused · ⌛ expired · 🔴 node down", "🟢 в сети · ⚪ не в сети · ⏸ пауза · ⌛ срок истёк · 🔴 нода не работает"},
	"ui.updated":        {"<i>updated %s</i>", "<i>обновлено %s</i>"},
	"ui.nostatus":       {"no status", "нет статуса"},
	"ui.node.down":      {"node not running", "нода не запущена"},
	"ui.node.stopped":   {"stopped", "остановлена"},
	"ui.node.up":        {"running %s", "работает %s"},
	"ui.doctor.title":   {"🩺 <b>Checks</b>", "🩺 <b>Проверка</b>"},
	"ui.doctor.sum":     {"Problems: %d, warnings: %d", "Проблем: %d, предупреждений: %d"},
	"ui.client.access":  {"Access: %s", "Доступ: %s"},
	"ui.client.node":    {"Node: %s", "Нода: %s"},
	"ui.client.client":  {"Client: %s", "Клиент: %s"},
	"ui.client.traffic": {"Traffic since the node started: ↓%s ↑%s", "Трафик с запуска ноды: ↓%s ↑%s"},
	"ui.client.created": {"Created: %s", "Создан: %s"},
	"ui.add.prompt": {"➕ <b>New client</b>\nSend in one message a name and the link to a <b>new</b> mail.ru document (anyone with the link can edit), and an expiry if needed:\n<code>guest https://cloud.mail.ru/public/XXXX/YYYY 30d</code>\n\nName: lowercase latin letters, digits, dashes.\nExpiry: 30d, 2w, 12h, 2026-12-31 or never (the default).",
		"➕ <b>Новый клиент</b>\nПришлите одним сообщением имя и ссылку на <b>новый</b> документ mail.ru (доступ по ссылке с редактированием), при необходимости — срок:\n<code>guest https://cloud.mail.ru/public/XXXX/YYYY 30d</code>\n\nИмя: строчные латинские буквы, цифры, дефис.\nСрок: 30d, 2w, 12h, 2026-12-31 или never (по умолчанию)."},
	"ui.add.done":       {"✅ Client <b>%s</b> added; its node is starting. 📱 QR and link: for the client's app.", "✅ Клиент <b>%s</b> добавлен, нода запускается. 📱 QR и ссылка — для приложения клиента."},
	"ui.add.nostart":    {"added %s, but starting its node failed", "клиент %s добавлен, но нода не запустилась"},
	"ui.resume.expired": {"access expired: extend it first (📅 +30 days or ♾ No limit)", "срок доступа истёк: сначала продлите его (📅 +30 дней или ♾ Бессрочно)"},
	"ui.revoke.ask":     {"🗑 Revoke <b>%s</b>?\nIts node stops and its key is deleted for good: the client will need a new link.", "🗑 Отозвать <b>%s</b>?\nНода остановится, ключ удалится навсегда: клиенту понадобится новая ссылка."},
	"ui.revoke.done":    {"🗑 <b>%s</b> revoked.", "🗑 Клиент <b>%s</b> отозван."},
	"ui.restart.ask":    {"🔁 Recreate egress and every node?\nClients lose the channel for about a minute, and the world tunnel may take 1–3 minutes to pick a server.", "🔁 Пересоздать egress и все ноды?\nКлиенты потеряют канал примерно на минуту, мировой туннель может 1–3 минуты подбирать сервер."},
	"ui.restart.done":   {"🔁 Egress and every node recreated.", "🔁 Egress и все ноды пересозданы."},
	"ui.expired.button": {"The button is too old: open the screen again", "Кнопка устарела: откройте экран заново"},
	"ui.settings":       {"⚙️ <b>Settings</b>\nLanguage: %s", "⚙️ <b>Настройки</b>\nЯзык: %s"},
	"lang.name":         {"English", "Русский"},
	"ui.sent":           {"Sent", "Отправлено"},
	"ui.unknown":        {"Unknown command: /help", "Неизвестная команда: /help"},
	"ui.help":           {"🛰 <b>ReFlux</b>: alerts when something breaks or changes, and client management. Buttons do the same as the commands.", "🛰 <b>ReFlux</b>: уведомления, когда что-то ломается или меняется, и управление клиентами. Кнопки делают то же, что команды."},
	"ui.show.photo":     {"<b>%s</b>: scan in the OpenFlux app (deleted in %d min)", "<b>%s</b>: отсканируйте в приложении OpenFlux (удалится через %d мин)"},
	"ui.show.failed":    {"could not send the QR code", "не удалось отправить QR-код"},
	"ui.show.text": {"<b>%s</b>: access to the channel, pass it to its owner only (deleted in %d min).\nAccess: %s\n\nManual entry:\nTransport: <code>%s</code>\nDocument URL: <code>%s</code>\nEncryption key: <code>%s</code>\nLegacy codec: off\n\nLink:\n<code>%s</code>",
		"<b>%s</b>: доступ к каналу, передайте только его владельцу (удалится через %d мин).\nДоступ: %s\n\nРучной ввод:\nТранспорт: <code>%s</code>\nСсылка на документ: <code>%s</code>\nКлюч шифрования: <code>%s</code>\nLegacy codec: выключен\n\nСсылка:\n<code>%s</code>"},
	"ui.usage.show":   {"/show &lt;name&gt;", "/show &lt;имя&gt;"},
	"ui.usage.pause":  {"/pause &lt;name&gt;", "/pause &lt;имя&gt;"},
	"ui.usage.resume": {"/resume &lt;name&gt;", "/resume &lt;имя&gt;"},
	"ui.usage.expire": {"/expire &lt;name&gt; &lt;never|2026-12-31|30d|2w|12h&gt;", "/expire &lt;имя&gt; &lt;never|2026-12-31|30d|2w|12h&gt;"},
	"ui.usage.revoke": {"/revoke &lt;name&gt;", "/revoke &lt;имя&gt;"},
	"ui.usage.logs":   {"/logs &lt;name|egress&gt;", "/logs &lt;имя|egress&gt;"},

	// buttons
	"b.refresh":         {"🔄 Refresh", "🔄 Обновить"},
	"b.doctor":          {"🩺 Checks", "🩺 Проверка"},
	"b.clients":         {"👥 Clients", "👥 Клиенты"},
	"b.add":             {"➕ Add", "➕ Добавить"},
	"b.settings":        {"⚙️ Settings", "⚙️ Настройки"},
	"b.home":            {"🏠 Home", "🏠 Главная"},
	"b.back":            {"⬅️ Back", "⬅️ Назад"},
	"b.restart":         {"🔁 Restart all", "🔁 Перезапустить всё"},
	"b.qr":              {"📱 QR and link", "📱 QR и ссылка"},
	"b.pause":           {"⏸ Pause", "⏸ Пауза"},
	"b.resume":          {"▶️ Resume", "▶️ Включить"},
	"b.plus30":          {"📅 +30 days", "📅 +30 дней"},
	"b.never":           {"♾ No limit", "♾ Бессрочно"},
	"b.logs":            {"📜 Log", "📜 Журнал"},
	"b.revoke":          {"🗑 Revoke", "🗑 Отозвать"},
	"b.cancel":          {"✖️ Cancel", "✖️ Отмена"},
	"b.confirm.revoke":  {"🗑 Yes, revoke %s", "🗑 Да, отозвать %s"},
	"b.confirm.restart": {"🔁 Yes, restart", "🔁 Да, перезапустить"},

	// command menu
	"cmd.start":    {"home: tunnels and clients", "главная: туннели и клиенты"},
	"cmd.doctor":   {"every check", "все проверки"},
	"cmd.clients":  {"clients", "клиенты"},
	"cmd.add":      {"new client", "новый клиент"},
	"cmd.show":     {"<name>: QR and link for the app", "<имя>: QR и ссылка для приложения"},
	"cmd.pause":    {"<name>: switch a client off", "<имя>: выключить клиента"},
	"cmd.resume":   {"<name>: switch it back on", "<имя>: включить клиента"},
	"cmd.expire":   {"<name> <30d|2026-12-31|never>: access expiry", "<имя> <30d|2026-12-31|never>: срок доступа"},
	"cmd.revoke":   {"<name>: delete a client's key for good", "<имя>: удалить ключ клиента навсегда"},
	"cmd.restart":  {"recreate egress and every node", "пересоздать egress и все ноды"},
	"cmd.logs":     {"<name|egress>: the last log lines", "<имя|egress>: последние строки журнала"},
	"cmd.settings": {"language", "язык"},
	"cmd.help":     {"what the bot does", "что умеет бот"},
}
