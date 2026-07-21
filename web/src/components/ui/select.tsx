import * as React from 'react'
import { Select as BaseSelect } from '@base-ui-components/react/select'
import { Check, ChevronDown } from 'lucide-react'

import { cn } from '#/lib/utils'

const Select = BaseSelect.Root
const SelectValue = BaseSelect.Value
const SelectGroup = BaseSelect.Group

const SelectTrigger = React.forwardRef<
  React.ComponentRef<typeof BaseSelect.Trigger>,
  React.ComponentPropsWithoutRef<typeof BaseSelect.Trigger>
>(({ className, children, ...props }, ref) => (
  <BaseSelect.Trigger
    ref={ref}
    className={cn(
      'flex h-9 w-full items-center justify-between rounded-md border border-input bg-transparent px-3 py-2 text-sm whitespace-nowrap shadow-sm ring-offset-background placeholder:text-muted-foreground focus:ring-1 focus:ring-ring focus:outline-none disabled:cursor-not-allowed disabled:opacity-50 [&>span]:line-clamp-1',
      className,
    )}
    {...props}
  >
    {children}
    <BaseSelect.Icon>
      <ChevronDown className="h-4 w-4 opacity-50" />
    </BaseSelect.Icon>
  </BaseSelect.Trigger>
))
SelectTrigger.displayName = 'SelectTrigger'

const SelectContent = React.forwardRef<
  React.ComponentRef<typeof BaseSelect.Positioner>,
  React.ComponentPropsWithoutRef<typeof BaseSelect.Positioner> & {
    popupClassName?: string
  }
>(({ className, popupClassName, children, ...props }, ref) => (
  <BaseSelect.Portal>
    <BaseSelect.Positioner
      ref={ref}
      className={cn('z-50', className)}
      // Default (true) overlays the popup on the trigger and shifts it so the
      // selected item sits over the trigger — so a mid-list selection pushes the
      // popup up over the header. Anchor it below the trigger like a normal
      // dropdown instead.
      alignItemWithTrigger={false}
      side="bottom"
      align="start"
      sideOffset={4}
      {...props}
    >
      <BaseSelect.Popup
        className={cn(
          'max-h-(--available-height) min-w-(--anchor-width) overflow-hidden rounded-md border bg-popover text-popover-foreground shadow-md',
          popupClassName,
        )}
      >
        <BaseSelect.ScrollUpArrow className="flex h-6 cursor-default items-center justify-center bg-popover" />
        <div className="p-1">{children}</div>
        <BaseSelect.ScrollDownArrow className="flex h-6 cursor-default items-center justify-center bg-popover" />
      </BaseSelect.Popup>
    </BaseSelect.Positioner>
  </BaseSelect.Portal>
))
SelectContent.displayName = 'SelectContent'

const SelectItem = React.forwardRef<
  React.ComponentRef<typeof BaseSelect.Item>,
  React.ComponentPropsWithoutRef<typeof BaseSelect.Item>
>(({ className, children, ...props }, ref) => (
  <BaseSelect.Item
    ref={ref}
    className={cn(
      'relative flex w-full cursor-default items-center rounded-sm py-1.5 pr-8 pl-2 text-sm outline-none select-none focus:bg-accent focus:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground',
      className,
    )}
    {...props}
  >
    <BaseSelect.ItemText>{children}</BaseSelect.ItemText>
    <span className="absolute right-2 flex h-3.5 w-3.5 items-center justify-center">
      <BaseSelect.ItemIndicator>
        <Check className="h-4 w-4" />
      </BaseSelect.ItemIndicator>
    </span>
  </BaseSelect.Item>
))
SelectItem.displayName = 'SelectItem'

export { Select, SelectValue, SelectGroup, SelectTrigger, SelectContent, SelectItem }
