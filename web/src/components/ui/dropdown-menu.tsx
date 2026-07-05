import * as React from 'react'
import { Menu as BaseMenu } from '@base-ui-components/react/menu'

import { cn } from '#/lib/utils.ts'

const DropdownMenu = BaseMenu.Root
const DropdownMenuGroup = BaseMenu.Group

const DropdownMenuTrigger = React.forwardRef<
  React.ComponentRef<typeof BaseMenu.Trigger>,
  React.ComponentPropsWithoutRef<typeof BaseMenu.Trigger> & { asChild?: boolean }
>(({ asChild, children, ...props }, ref) => {
  if (asChild && React.isValidElement(children)) {
    return (
      <BaseMenu.Trigger
        ref={ref}
        // Our triggers are always a real <button> (Button / SidebarMenuButton),
        // so tell Base UI that — nativeButton={false} made it warn and apply
        // the wrong a11y attributes on every dropdown.
        nativeButton
        render={children as React.ReactElement<Record<string, unknown>>}
        {...props}
      />
    )
  }
  return (
    <BaseMenu.Trigger ref={ref} {...props}>
      {children}
    </BaseMenu.Trigger>
  )
})
DropdownMenuTrigger.displayName = 'DropdownMenuTrigger'

const DropdownMenuContent = React.forwardRef<
  React.ComponentRef<typeof BaseMenu.Positioner>,
  React.ComponentPropsWithoutRef<typeof BaseMenu.Positioner> & {
    popupClassName?: string
  }
>(({ className, popupClassName, children, sideOffset = 4, ...props }, ref) => (
  <BaseMenu.Portal>
    <BaseMenu.Positioner
      ref={ref}
      sideOffset={sideOffset}
      className={cn('z-50', className)}
      {...props}
    >
      <BaseMenu.Popup
        className={cn(
          'min-w-[8rem] overflow-hidden rounded-md border bg-popover p-1 text-popover-foreground shadow-md outline-none',
          popupClassName,
        )}
      >
        {children}
      </BaseMenu.Popup>
    </BaseMenu.Positioner>
  </BaseMenu.Portal>
))
DropdownMenuContent.displayName = 'DropdownMenuContent'

const DropdownMenuItem = React.forwardRef<
  React.ComponentRef<typeof BaseMenu.Item>,
  React.ComponentPropsWithoutRef<typeof BaseMenu.Item> & { asChild?: boolean }
>(({ className, asChild, children, ...props }, ref) => {
  const itemClassName = cn(
    'relative flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none select-none focus:bg-accent focus:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground',
    className,
  )
  if (asChild && React.isValidElement(children)) {
    return (
      <BaseMenu.Item
        ref={ref}
        className={itemClassName}
        render={children as React.ReactElement<Record<string, unknown>>}
        {...props}
      />
    )
  }
  return (
    <BaseMenu.Item ref={ref} className={itemClassName} {...props}>
      {children}
    </BaseMenu.Item>
  )
})
DropdownMenuItem.displayName = 'DropdownMenuItem'

const DropdownMenuSeparator = React.forwardRef<
  HTMLDivElement,
  React.HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    role="separator"
    className={cn('-mx-1 my-1 h-px bg-muted', className)}
    {...props}
  />
))
DropdownMenuSeparator.displayName = 'DropdownMenuSeparator'

export {
  DropdownMenu,
  DropdownMenuGroup,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
}
