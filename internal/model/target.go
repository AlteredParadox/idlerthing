package model

// TargetNameSQL is a SQL expression resolving a polymorphic service's
// display name from (service_id, service_type) columns of table alias a.
const TargetNameSQL = `COALESCE(
	(SELECT hostname FROM servers WHERE id = a.service_id AND a.service_type = 1),
	(SELECT main_domain FROM shared_hosting WHERE id = a.service_id AND a.service_type = 2),
	(SELECT main_domain FROM reseller_hosting WHERE id = a.service_id AND a.service_type = 3),
	(SELECT domain FROM domains WHERE id = a.service_id AND a.service_type = 4),
	(SELECT name FROM misc_services WHERE id = a.service_id AND a.service_type = 5),
	(SELECT hostname FROM seedboxes WHERE id = a.service_id AND a.service_type = 6),
	'(deleted)'
)`

// ServiceBasePath maps a service_type to its web base path.
func ServiceBasePath(serviceType int) string {
	switch serviceType {
	case ServiceServer:
		return "/servers"
	case ServiceShared:
		return "/shared"
	case ServiceReseller:
		return "/reseller"
	case ServiceDomain:
		return "/domains"
	case ServiceMisc:
		return "/misc"
	case ServiceSeedbox:
		return "/seedboxes"
	default:
		return ""
	}
}

// ServiceTypeLabel maps a service_type to a short badge label.
func ServiceTypeLabel(serviceType int) string {
	switch serviceType {
	case ServiceServer:
		return "Server"
	case ServiceShared:
		return "Shared"
	case ServiceReseller:
		return "Reseller"
	case ServiceDomain:
		return "Domain"
	case ServiceMisc:
		return "Misc"
	case ServiceSeedbox:
		return "Seedbox"
	default:
		return "?"
	}
}
