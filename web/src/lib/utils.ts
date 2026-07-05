import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// Capitalize the first letter. For enum-ish values (roles, invite statuses)
// stored lowercase but shown as sentence case in the UI, so casing stays
// consistent with the rest of the interface.
export function capitalize(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s
}
