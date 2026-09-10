package httpx

import (
	"strings"

	"github.com/playxdev/dormapi/internal/repo"
)

// What the Official Account says.
//
// Kept apart from the routing so the wording can be read as wording. Thai,
// plain, and short: this is read on a phone, in a chat, usually by somebody who
// wanted a person and got a machine — so every message ends by pointing
// somewhere that leads to one.

const msgWelcome = "ยินดีต้อนรับสู่ dorm.place\n\n" +
	"เปิดเมนูด้านล่างเพื่อดูบิล แจ้งซ่อม และดูมิเตอร์ห้องของคุณ\n" +
	"ถ้ายังไม่ได้ผูกห้อง ให้สแกน QR ที่ได้รับจากผู้ดูแลหอพัก"

const msgNoAccount = "ยังไม่พบบัญชีของคุณในระบบ\n\n" +
	"กรุณาเปิดแอปจากเมนูด้านล่างและเข้าสู่ระบบด้วย LINE ก่อน 1 ครั้ง"

const msgNoRoom = "บัญชีของคุณยังไม่ได้ผูกกับห้องพัก\n\n" +
	"สแกน QR ที่ได้รับจากผู้ดูแลหอพัก หรือเปิดลิงก์เชิญที่ได้รับ เพื่อผูกห้องก่อน"

const msgWhichOperator = "คุณเช่าอยู่มากกว่าหนึ่งที่ กรุณาเลือกว่าต้องการติดต่อที่ไหน"

func msgUseTheApp(buildingName, roomNumber string) string {
	where := strings.TrimSpace(buildingName)
	if roomNumber != "" {
		where += " ห้อง " + roomNumber
	}
	return "ระบบนี้ยังตอบข้อความไม่ได้\n\n" +
		"เปิดเมนูด้านล่างเพื่อดูบิล แจ้งซ่อม และดูมิเตอร์ของ " + where + "\n" +
		"ถ้าต้องการติดต่อผู้ดูแล พิมพ์ ADMIN"
}

// msgContact answers the rich menu's ADMIN button.
//
// The address is the operator's, and it is the most useful thing the schema
// holds: `building` keeps no telephone number, so a number cannot be given
// here without one being stored there first.
func msgContact(c *repo.Contact) string {
	var b strings.Builder
	b.WriteString("ติดต่อผู้ดูแลหอพัก\n\n")
	b.WriteString(c.OperatorName)
	if c.BuildingName != "" {
		b.WriteString("\n" + c.BuildingName)
	}
	if c.RoomNumber != "" {
		b.WriteString(" ห้อง " + c.RoomNumber)
	}
	if strings.TrimSpace(c.Address) != "" {
		b.WriteString("\n" + c.Address)
	}
	b.WriteString("\n\nเรื่องบิล แจ้งซ่อม และมิเตอร์ ทำได้เองในแอปจากเมนูด้านล่าง")
	return b.String()
}
