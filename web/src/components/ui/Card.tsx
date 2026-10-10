import { cn } from '@/lib/utils';
import type { ReactNode } from 'react';

interface CardProps {
  children: ReactNode;
  className?: string;
  hover?: boolean;
}

export function Card({ children, className, hover }: CardProps) {
  return (
    <div
      className={cn(
        'rounded-xl p-5 transition-all duration-200 animate-float-in',
        'bg-white/80 backdrop-blur-md border border-gray-200/90',
        'dark:bg-[rgba(0,0,0,0.55)] dark:backdrop-blur-xl dark:border-white/[0.08]',
        hover &&
          'hover:-translate-y-1 hover:shadow-lg hover:shadow-black/5 dark:hover:shadow-black/30 cursor-pointer',
        className
      )}
    >
      {children}
    </div>
  );
}
